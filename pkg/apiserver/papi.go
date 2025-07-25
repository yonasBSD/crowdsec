package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/tomb.v2"

	"github.com/crowdsecurity/go-cs-lib/trace"

	"github.com/crowdsecurity/crowdsec/pkg/apiclient"
	"github.com/crowdsecurity/crowdsec/pkg/csconfig"
	"github.com/crowdsecurity/crowdsec/pkg/database"
	"github.com/crowdsecurity/crowdsec/pkg/longpollclient"
	"github.com/crowdsecurity/crowdsec/pkg/models"
	"github.com/crowdsecurity/crowdsec/pkg/types"
)

const (
	SyncInterval = time.Second * 10
	PapiPullKey  = "papi:last_pull"
)

var operationMap = map[string]func(*Message, *Papi, bool) error{
	"decision":   DecisionCmd,
	"alert":      AlertCmd,
	"management": ManagementCmd,
}

type Header struct {
	OperationType string    `json:"operation_type"`
	OperationCmd  string    `json:"operation_cmd"`
	Timestamp     time.Time `json:"timestamp"`
	Message       string    `json:"message"`
	UUID          string    `json:"uuid"`
	Source        *Source   `json:"source"`
	Destination   string    `json:"destination"`
}

type Source struct {
	User string `json:"user"`
}

type Message struct {
	Header *Header
	Data   any `json:"data"`
}

type OperationChannels struct {
	AddAlertChannel       chan []*models.Alert
	DeleteDecisionChannel chan []*models.Decision
}

type Papi struct {
	URL           string
	Client        *longpollclient.LongPollClient
	DBClient      *database.Client
	apiClient     *apiclient.ApiClient
	Channels      *OperationChannels
	mu            sync.Mutex
	pullTomb      tomb.Tomb
	syncTomb      tomb.Tomb
	SyncInterval  time.Duration
	consoleConfig *csconfig.ConsoleConfig
	Logger        *log.Entry
	apic          *apic
	stopChan      chan struct{}
}

type PapiPermCheckError struct {
	Error string `json:"error"`
}

type PapiPermCheckSuccess struct {
	Status     string   `json:"status"`
	Plan       string   `json:"plan"`
	Categories []string `json:"categories"`
}

func NewPAPI(apic *apic, dbClient *database.Client, consoleConfig *csconfig.ConsoleConfig, logLevel log.Level) (*Papi, error) {
	logger := log.New()
	if err := types.ConfigureLogger(logger, &logLevel); err != nil {
		return &Papi{}, fmt.Errorf("creating papi logger: %w", err)
	}

	papiUrl := *apic.apiClient.PapiURL
	papiUrl.Path = fmt.Sprintf("%s%s", types.PAPIVersion, types.PAPIPollUrl)

	longPollClient, err := longpollclient.NewLongPollClient(longpollclient.LongPollClientConfig{
		Url:        papiUrl,
		Logger:     logger,
		HttpClient: apic.apiClient.GetClient(),
	})
	if err != nil {
		return &Papi{}, fmt.Errorf("failed to create PAPI client: %w", err)
	}

	channels := &OperationChannels{
		AddAlertChannel:       apic.AlertsAddChan,
		DeleteDecisionChannel: make(chan []*models.Decision),
	}

	papi := &Papi{
		URL:           apic.apiClient.PapiURL.String(),
		Client:        longPollClient,
		DBClient:      dbClient,
		Channels:      channels,
		SyncInterval:  SyncInterval,
		mu:            sync.Mutex{},
		pullTomb:      tomb.Tomb{},
		syncTomb:      tomb.Tomb{},
		apiClient:     apic.apiClient,
		apic:          apic,
		consoleConfig: consoleConfig,
		Logger:        logger.WithFields(log.Fields{"interval": SyncInterval.Seconds(), "source": "papi"}),
		stopChan:      make(chan struct{}),
	}

	return papi, nil
}

func (p *Papi) handleEvent(event longpollclient.Event, sync bool) error {
	logger := p.Logger.WithField("request-id", event.RequestId)
	logger.Debugf("message received: %+v", event.Data)

	message := &Message{}
	if err := json.Unmarshal([]byte(event.Data), message); err != nil {
		return fmt.Errorf("polling papi message format is not compatible: %+v: %w", event.Data, err)
	}

	if message.Header == nil {
		return errors.New("no header in message, skipping")
	}

	if message.Header.Source == nil {
		return errors.New("no source user in header message, skipping")
	}

	operationFunc, ok := operationMap[message.Header.OperationType]
	if !ok {
		return fmt.Errorf("operation '%s' unknown, continue", message.Header.OperationType)
	}

	logger.Debugf("Calling operation '%s'", message.Header.OperationType)

	err := operationFunc(message, p, sync)
	if err != nil {
		return fmt.Errorf("'%s %s failed: %w", message.Header.OperationType, message.Header.OperationCmd, err)
	}

	return nil
}

func (p *Papi) GetPermissions(ctx context.Context) (PapiPermCheckSuccess, error) {
	httpClient := p.apiClient.GetClient()
	papiCheckUrl := fmt.Sprintf("%s%s%s", p.URL, types.PAPIVersion, types.PAPIPermissionsUrl)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, papiCheckUrl, http.NoBody)
	if err != nil {
		return PapiPermCheckSuccess{}, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return PapiPermCheckSuccess{}, fmt.Errorf("failed to get response: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errResp := PapiPermCheckError{}

		err = json.NewDecoder(resp.Body).Decode(&errResp)
		if err != nil {
			return PapiPermCheckSuccess{}, fmt.Errorf("failed to decode response: %w", err)
		}

		return PapiPermCheckSuccess{}, fmt.Errorf("unable to query PAPI : %s (%d)", errResp.Error, resp.StatusCode)
	}

	respBody := PapiPermCheckSuccess{}

	err = json.NewDecoder(resp.Body).Decode(&respBody)
	if err != nil {
		return PapiPermCheckSuccess{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return respBody, nil
}

func reverse(s []longpollclient.Event) []longpollclient.Event {
	a := make([]longpollclient.Event, len(s))
	copy(a, s)

	for i := len(a)/2 - 1; i >= 0; i-- {
		opp := len(a) - 1 - i
		a[i], a[opp] = a[opp], a[i]
	}

	return a
}

func (p *Papi) PullOnce(ctx context.Context, since time.Time, sync bool) error {
	events, err := p.Client.PullOnce(ctx, since)
	if err != nil {
		return err
	}

	reversedEvents := reverse(events) // PAPI sends events in the reverse order, which is not an issue when pulling them in real time, but here we need the correct order
	eventsCount := len(events)
	p.Logger.Infof("received %d events", eventsCount)

	for i, event := range reversedEvents {
		if err := p.handleEvent(event, sync); err != nil {
			p.Logger.WithField("request-id", event.RequestId).Errorf("failed to handle event: %s", err)
		}

		p.Logger.Debugf("handled event %d/%d", i, eventsCount)
	}

	p.Logger.Debugf("finished handling events")
	// Don't update the timestamp in DB, as a "real" LAPI might be running
	// Worst case, crowdsec will receive a few duplicated events and will discard them
	return nil
}

// PullPAPI is the long polling client for real-time decisions from PAPI
func (p *Papi) Pull(ctx context.Context) error {
	defer trace.CatchPanic("lapi/PullPAPI")
	p.Logger.Infof("Starting Polling API Pull")

	lastTimestamp := time.Time{}

	lastTimestampStr, err := p.DBClient.GetConfigItem(ctx, PapiPullKey)
	if err != nil {
		p.Logger.Warningf("failed to get last timestamp for papi pull: %s", err)
	}

	// value doesn't exist, it's first time we're pulling
	if lastTimestampStr == "" {
		binTime, err := lastTimestamp.MarshalText()
		if err != nil {
			return fmt.Errorf("failed to serialize last timestamp: %w", err)
		}

		if err := p.DBClient.SetConfigItem(ctx, PapiPullKey, string(binTime)); err != nil {
			p.Logger.Errorf("error setting papi pull last key: %s", err)
		} else {
			p.Logger.Debugf("config item '%s' set in database with value '%s'", PapiPullKey, string(binTime))
		}
	} else {
		if err := lastTimestamp.UnmarshalText([]byte(lastTimestampStr)); err != nil {
			return fmt.Errorf("failed to parse last timestamp: %w", err)
		}
	}

	tokenRefreshChan := p.apiClient.GetTokenRefreshChan()
	var papiChan chan longpollclient.Event // Chan is nil by default to block until PAPI actually establishes the connection
	papiCtx, cancel := context.WithCancel(ctx)

	currentSubscriptionType := p.apiClient.GetSubscriptionType()

	p.Logger.Debugf("current subscription type is %s", currentSubscriptionType)

	if currentSubscriptionType == apiclient.SubscriptionTypeEnterprise || currentSubscriptionType == apiclient.SubscriptionTypeSecOps {
		// If allowed to use PAPI, start it
		// Otherwise it will be started when the token is refreshed with an ent subscription
		p.Logger.Infof("Starting PAPI pull (since:%s)", lastTimestamp)
		papiChan = p.Client.Start(papiCtx, lastTimestamp)
	}

	for {
		select {
		case <-tokenRefreshChan:
			subType := p.apiClient.GetSubscriptionType()
			if subType == currentSubscriptionType {
				continue
			}
			currentSubscriptionType = subType
			p.Logger.Infof("Subscription type changed to %s", subType)
			switch subType {
			case apiclient.SubscriptionTypeEnterprise, apiclient.SubscriptionTypeSecOps:
				p.Logger.Infof("Starting PAPI pull (since:%s)", lastTimestamp)
				papiChan = p.Client.Start(papiCtx, lastTimestamp)
			default:
				// PAPI got started but the user downgraded (or removed the engine from the console)
				p.Logger.Info("Stopping PAPI because of plan downgrade or engine removal")
				cancel() // This will stop any ongoing PAPI pull
				p.Client.Stop()
				papiCtx, cancel = context.WithCancel(ctx) //nolint:fatcontext // Recreate the context if the pull is restarted
				papiChan = nil
				p.Logger.Debug("done stopping PAPI pull")
			}
		case event := <-papiChan:
			logger := p.Logger.WithField("request-id", event.RequestId)
			// update last timestamp in database
			newTime := time.Now().UTC()

			binTime, _ := newTime.MarshalText() // No need to check the error, time.Now().UTC() always returns a valid time

			lastTimestamp = newTime

			err = p.handleEvent(event, false)
			if err != nil {
				logger.Errorf("failed to handle event: %s", err)
				continue
			}

			if err := p.DBClient.SetConfigItem(ctx, PapiPullKey, string(binTime)); err != nil {
				// Killing PAPI is overkill if we cannot update the last timestamp
				logger.Errorf("failed to update last timestamp in database: %s", err)
			}

			logger.Debugf("set last timestamp to %s", newTime)
		case <-p.stopChan:
			cancel()
		}
	}

}

func (p *Papi) SyncDecisions(ctx context.Context) error {
	defer trace.CatchPanic("lapi/syncDecisionsToCAPI")

	var cache models.DecisionsDeleteRequest

	ticker := time.NewTicker(p.SyncInterval)
	p.Logger.Infof("Start decisions sync to CrowdSec Central API (interval: %s)", p.SyncInterval)

	for {
		select {
		case <-p.syncTomb.Dying(): // if one apic routine is dying, do we kill the others?
			p.Logger.Infof("sync decisions tomb is dying, sending cache (%d elements) before exiting", len(cache))

			if len(cache) == 0 {
				return nil
			}

			go p.SendDeletedDecisions(ctx, &cache)

			return nil
		case <-ticker.C:
			if len(cache) > 0 {
				p.mu.Lock()
				cacheCopy := cache
				cache = make([]models.DecisionsDeleteRequestItem, 0)
				p.mu.Unlock()
				p.Logger.Infof("sync decisions: %d deleted decisions to push", len(cacheCopy))

				go p.SendDeletedDecisions(ctx, &cacheCopy)
			}
		case deletedDecisions := <-p.Channels.DeleteDecisionChannel:
			if (p.consoleConfig.ShareManualDecisions != nil && *p.consoleConfig.ShareManualDecisions) || (p.consoleConfig.ConsoleManagement != nil && *p.consoleConfig.ConsoleManagement) {
				var tmpDecisions []models.DecisionsDeleteRequestItem

				p.Logger.Debugf("%d decisions deletion to add in cache", len(deletedDecisions))

				for _, decision := range deletedDecisions {
					tmpDecisions = append(tmpDecisions, models.DecisionsDeleteRequestItem(decision.UUID))
				}

				p.mu.Lock()
				cache = append(cache, tmpDecisions...)
				p.mu.Unlock()
			}
		}
	}
}

func (p *Papi) sendDeletedDecisionsBatch(ctx context.Context, decisions []models.DecisionsDeleteRequestItem) error {
	ctxBatch, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, _, err := p.apiClient.DecisionDelete.Add(ctxBatch, (*models.DecisionsDeleteRequest)(&decisions))
	if err != nil {
		return err
	}

	return nil
}

func (p *Papi) SendDeletedDecisions(ctx context.Context, cacheOrig *models.DecisionsDeleteRequest) {
	var cache []models.DecisionsDeleteRequestItem = *cacheOrig

	batchSize := 50

	for start := 0; start < len(cache); start += batchSize {
		end := min(start+batchSize, len(cache))

		if err := p.sendDeletedDecisionsBatch(ctx, cache[start:end]); err != nil {
			p.Logger.Errorf("sending deleted decisions to central API: %s", err)
			return
		}
	}
}

func (p *Papi) Shutdown() {
	p.Logger.Infof("Shutting down PAPI")
	p.syncTomb.Kill(nil)
	select {
	case p.stopChan <- struct{}{}: // Cancel any HTTP request still in progress
	default:
	}
	p.Client.Stop()
}
