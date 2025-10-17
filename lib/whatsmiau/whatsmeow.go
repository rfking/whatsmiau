package whatsmiau

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/verbeux-ai/whatsmiau/env"
	"github.com/verbeux-ai/whatsmiau/interfaces"
	"github.com/verbeux-ai/whatsmiau/lib/storage/gcs"
	"github.com/verbeux-ai/whatsmiau/models"
	"github.com/verbeux-ai/whatsmiau/repositories/instances"
	"github.com/verbeux-ai/whatsmiau/services"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.uber.org/zap"
	"golang.org/x/net/context"
)

type Whatsmiau struct {
	clients          *xsync.Map[string, *whatsmeow.Client]
	container        *sqlstore.Container
	logger           waLog.Logger
	repo             interfaces.InstanceRepository
	qrCache          *xsync.Map[string, string]
	observerRunning  *xsync.Map[string, bool]
	instanceCache    *xsync.Map[string, models.Instance]
	emitter          chan emitter
	httpClient       *http.Client
	fileStorage      interfaces.Storage
	handlerSemaphore chan struct{}
}

var instance *Whatsmiau
var mu = &sync.Mutex{}

func Get() *Whatsmiau {
	mu.Lock()
	defer mu.Unlock()
	return instance
}

func LoadMiau(ctx context.Context, container *sqlstore.Container) {
	mu.Lock()
	defer mu.Unlock()
	deviceStore, err := container.GetAllDevices(ctx)
	if err != nil {
		panic(err)
	}

	level := "INFO"
	if env.Env.DebugWhatsmeow {
		level = "DEBUG"
	}

	repo := instances.NewRedis(services.Redis())
	instanceList, err := repo.List(ctx, "")
	if err != nil {
		zap.L().Fatal("failed to list instances", zap.Error(err))
	}

	instanceByRemoteJid := make(map[string]models.Instance)
	for _, inst := range instanceList {
		if len(inst.RemoteJID) <= 0 {
			continue
		}

		instanceByRemoteJid[inst.RemoteJID] = inst
	}

	clients := xsync.NewMap[string, *whatsmeow.Client]()

	clientLog := waLog.Stdout("Client", level, false)
	for _, device := range deviceStore {
		client := whatsmeow.NewClient(device, clientLog)
		if client.Store.ID == nil {
			_ = client.Logout(context.Background())
			client.Disconnect()
			if err := container.DeleteDevice(context.Background(), client.Store); err != nil {
				zap.L().Error("failed to delete device", zap.Error(err))
			}
			continue
		}

		instanceFound, ok := instanceByRemoteJid[client.Store.ID.String()]
		if ok {
			configProxy(client, instanceFound.InstanceProxy)
			clients.Store(instanceFound.ID, client)
			if client.IsLoggedIn() {
				if err := client.Connect(); err != nil {
					zap.L().Error("failed to connect connected device", zap.Error(err), zap.String("jid", client.Store.ID.String()))
				}
			}
		} else {
			_ = client.Logout(context.Background())
			client.Disconnect()
			if err := container.DeleteDevice(context.Background(), client.Store); err != nil {
				zap.L().Error("failed to delete device", zap.Error(err))
			}
		}
	}

	var storage interfaces.Storage
	if env.Env.GCSEnabled {
		storage, err = gcs.New(env.Env.GCSBucket)
		if err != nil {
			zap.L().Panic("failed to create GCS storage", zap.Error(err))
		}
	}

	instance = &Whatsmiau{
		clients:         clients,
		container:       container,
		logger:          clientLog,
		repo:            repo,
		qrCache:         xsync.NewMap[string, string](),
		instanceCache:   xsync.NewMap[string, models.Instance](),
		observerRunning: xsync.NewMap[string, bool](),
		emitter:         make(chan emitter, env.Env.EmitterBufferSize),
		httpClient: &http.Client{
			Timeout: time.Second * 30, // TODO: load from env
		},
		fileStorage:      storage,
		handlerSemaphore: make(chan struct{}, env.Env.HandlerSemaphoreSize),
	}

	go instance.startEmitter()

	clients.Range(func(id string, client *whatsmeow.Client) bool {
		zap.L().Info("stating event handler", zap.String("jid", client.Store.ID.String()))
		client.AddEventHandler(instance.Handle(id))
		return true
	})

}

func (s *Whatsmiau) Connect(ctx context.Context, id string) (string, error) {
	client, ok := s.clients.Load(id)
	if !ok {
		device := s.container.NewDevice()
		client = whatsmeow.NewClient(device, s.logger)
		s.clients.Store(id, client)
	}

	if client.IsLoggedIn() {
		return "", nil
	}

	if client.Store != nil && client.Store.ID != nil {
		if err := client.Logout(ctx); err != nil {
			zap.L().Debug("failed to logout", zap.String("jid", client.Store.ID.String()))
		}
		client.Disconnect()
		if err := s.container.DeleteDevice(ctx, client.Store); err != nil {
			zap.L().Debug("failed to delete device", zap.String("jid", client.Store.ID.String()))
		}

		device := s.container.NewDevice()
		client = whatsmeow.NewClient(device, s.logger)
		s.clients.Store(id, client)
	}

	if qr, ok := s.qrCache.Load(id); ok {
		return qr, nil
	}

	qrCode, err := s.observeAndQrCode(ctx, id, client)
	if err != nil {
		return "", err
	}

	return qrCode, nil
}

func (s *Whatsmiau) observeConnection(client *whatsmeow.Client, id string) {
	if _, ok := s.observerRunning.Load(id); ok {
		zap.L().Debug("observer connection already running", zap.String("id", id))
		return
	}

	zap.L().Debug("starting observer connection", zap.String("id", id))
	s.observerRunning.Store(id, true)
	defer func() {
		zap.L().Debug("stopping observer connection", zap.String("id", id))
		s.observerRunning.Delete(id)
		s.qrCache.Delete(id)
	}()
	ctx, cancel := context.WithTimeout(context.TODO(), time.Minute*2)
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		zap.L().Error("failed to observe QR Code", zap.Error(err))
		return
	}

	if !client.IsConnected() {
		zap.L().Debug("client is not connected, connecting", zap.String("id", id))
		instanceFound := s.getInstanceCached(id)
		configProxy(client, instanceFound.InstanceProxy)
		if err := client.Connect(); err != nil {
			zap.L().Error("failed to connect", zap.Error(err))
			return
		}
	}

	zap.L().Debug("waiting for QR channel event", zap.String("id", id))
	for {
		select {
		case <-ctx.Done(): // QR code expiration
			zap.L().Debug("QR code context is done", zap.String("id", id), zap.Error(ctx.Err()))
			_ = client.Logout(context.Background())
			client.Disconnect()
			if err := s.container.DeleteDevice(context.Background(), client.Store); err != nil {
				zap.L().Error("failed to delete device", zap.Error(err))
			}
			s.clients.Delete(id)
			zap.L().Info("QR code context is done", zap.String("id", id), zap.Error(ctx.Err()))
			return
		case evt, ok := <-qrChan:
			if !ok { // closed qr chan
				zap.L().Debug("QR channel closed", zap.String("id", id))
				cancel()
				return
			}
			zap.L().Debug("received QR channel event", zap.String("id", id), zap.Any("evt", evt))
			if evt.Event == "code" {
				s.qrCache.Store(id, evt.Code)
			} else {
				zap.L().Info("device connected successfully", zap.String("id", id))
				if client.Store.ID == nil {
					zap.L().Error("jid is nil after login", zap.String("id", id), zap.Any("evt", evt))
				} else {
					client.RemoveEventHandlers()
					client.AddEventHandler(s.Handle(id))
					if _, err := s.repo.Update(context.Background(), id, &models.Instance{
						RemoteJID: client.Store.ID.String(),
					}); err != nil {
						zap.L().Error("failed to update instance after login", zap.Error(err))
					}
				}
				cancel()
				return
			}
		}
	}
}

func (s *Whatsmiau) observeAndQrCode(ctx context.Context, id string, client *whatsmeow.Client) (string, error) {
	ctx, c := context.WithTimeout(ctx, 15*time.Second)
	defer c()

	zap.L().Debug("starting observe and qr code", zap.String("id", id))
	go s.observeConnection(client, id)

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			qrCode, ok := s.qrCache.Load(id)
			if ok && len(qrCode) > 0 {
				zap.L().Debug("got qr code from cache", zap.String("id", id))
				return qrCode, nil
			}
		case <-ctx.Done():
			zap.L().Debug("observe and qr code context done", zap.String("id", id), zap.Error(ctx.Err()))
			return "", nil
		}
	}
}

func (s *Whatsmiau) Status(id string) (Status, error) {
	client, ok := s.clients.Load(id)
	if !ok {
		return Closed, nil
	}

	if client.IsConnected() && client.IsLoggedIn() {
		return Connected, nil
	}

	// If not connected, but we have a QR code, the state is QrCode
	if _, ok := s.qrCache.Load(id); ok && client.IsConnected() {
		return QrCode, nil
	}

	if client.IsLoggedIn() {
		return Connecting, nil
	}

	return Closed, nil
}

func (s *Whatsmiau) Logout(ctx context.Context, id string) error {
	client, ok := s.clients.Load(id)
	if !ok {
		zap.L().Warn("logout: client does not exist", zap.String("id", id))
		return nil
	}

	return client.Logout(ctx)
}

func (s *Whatsmiau) Disconnect(id string) error {
	client, ok := s.clients.Load(id)
	if !ok {
		zap.L().Warn("failed to disconnect (device not loaded)", zap.String("id", id))
		return nil
	}

	client.Disconnect()
	if err := s.container.DeleteDevice(context.Background(), client.Store); err != nil {
		zap.L().Error("failed to delete device", zap.Error(err))
	}

	s.clients.Delete(id)
	s.qrCache.Delete(id)
	return nil
}

func (s *Whatsmiau) GetJidLid(ctx context.Context, id string, jid types.JID) (string, string) {
	newJid, newLid := s.extractJidLid(ctx, id, jid)
	if strings.HasSuffix(newJid, "@lid") {
		newLid = newJid
	}

	return newJid, newLid
}

func (s *Whatsmiau) extractJidLid(ctx context.Context, id string, jid types.JID) (string, string) {
	client, ok := s.clients.Load(id)
	if !ok {
		return jid.ToNonAD().String(), ""
	}

	if jid.Server == types.DefaultUserServer {
		lid, err := client.Store.LIDs.GetLIDForPN(ctx, jid)
		if err != nil {
			zap.L().Warn("failed to get lid from store", zap.String("id", id), zap.Error(err))
		}

		return jid.ToNonAD().String(), lid.ToNonAD().String()
	}

	if jid.Server == types.HiddenUserServer {
		lidString := jid.ToNonAD().String()
		pnJID, err := client.Store.LIDs.GetPNForLID(ctx, jid)
		if err != nil {
			zap.L().Warn("failed to get pn for lid", zap.Stringer("lid", jid), zap.Error(err))
			return jid.ToNonAD().String(), lidString
		}

		if !pnJID.IsEmpty() {
			return pnJID.ToNonAD().String(), lidString
		}

		return lidString, lidString
	}

	return jid.ToNonAD().String(), ""
}
