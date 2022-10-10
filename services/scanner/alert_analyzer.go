package scanner

import (
	"context"
	"time"

	"github.com/forta-network/forta-core-go/clients/health"
	"github.com/forta-network/forta-core-go/domain"
	"github.com/forta-network/forta-core-go/protocol/alerthash"
	"github.com/forta-network/forta-core-go/utils"
	"github.com/forta-network/forta-node/clients/messaging"
	"github.com/forta-network/forta-node/metrics"

	"github.com/golang/protobuf/jsonpb"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/forta-network/forta-core-go/protocol"
	"github.com/forta-network/forta-node/clients"
)

// AlertAnalyzerService reads TX info, calls agents, and emits results
type AlertAnalyzerService struct {
	ctx           context.Context
	cfg           AlertAnalyzerServiceConfig
	publisherNode protocol.PublisherNodeClient

	lastInputActivity  health.TimeTracker
	lastOutputActivity health.TimeTracker
}

type AlertAnalyzerServiceConfig struct {
	AlertChannel <-chan *domain.AlertEvent
	AlertSender  clients.AlertSender
	AgentPool    AgentPool
	MsgClient    clients.MessageClient
}

func (aas *AlertAnalyzerService) publishMetrics(result *AlertResult) {
	m := metrics.GetAlertMetrics(result.AgentConfig, result.Response, result.Timestamps)
	aas.cfg.MsgClient.PublishProto(messaging.SubjectMetricAgent, &protocol.AgentMetricList{Metrics: m})
}

func (aas *AlertAnalyzerService) findingToAlert(result *AlertResult, ts time.Time, f *protocol.Finding) (*protocol.Alert, error) {
	alertID := alerthash.ForBlockAlert(
		&alerthash.Inputs{
			Alert:   result.Request.Event,
			Finding: f,
			BotInfo: alerthash.BotInfo{
				BotImage: result.AgentConfig.Image,
				BotID:    result.AgentConfig.ID,
			},
		},
	)

	chainId, err := utils.HexToBigInt(result.Request.Event.Network.ChainId)
	if err != nil {
		return nil, err
	}
	tags := map[string]string{
		"agentImage": result.AgentConfig.Image,
		"agentId":    result.AgentConfig.ID,
		"chainId":    chainId.String(),
	}

	alertType := protocol.AlertType_PRIVATE
	if !f.Private && !result.Response.Private {
		alertType = protocol.AlertType_BLOCK
	}
	return &protocol.Alert{
		Id:         alertID,
		Finding:    f,
		Timestamp:  ts.Format(utils.AlertTimeFormat),
		Type:       alertType,
		Agent:      result.AgentConfig.ToAgentInfo(),
		Tags:       tags,
		Timestamps: result.Timestamps.ToMessage(),
	}, nil
}

func (aas *AlertAnalyzerService) Start() error {
	// Gear 2: receive result from agent
	go func() {
		for result := range aas.cfg.AgentPool.AlertResults() {
			ts := time.Now().UTC()

			m := jsonpb.Marshaler{}
			resStr, err := m.MarshalToString(result.Response)
			if err != nil {
				log.Error("error marshaling response", err)
				continue
			}
			log.Debugf(resStr)

			rt := &clients.AgentRoundTrip{
				AgentConfig:       result.AgentConfig,
				EvalAlertRequest:  result.Request,
				EvalAlertResponse: result.Response,
			}

			if len(result.Response.Findings) == 0 {
				if err := aas.cfg.AlertSender.NotifyWithoutAlert(
					rt, result.Timestamps,
				); err != nil {
					log.WithError(err).Panic("failed to notify without alert")
				}
			}

			for _, f := range result.Response.Findings {
				alert, err := aas.findingToAlert(result, ts, f)
				if err != nil {
					log.WithError(err).Error("failed to transform finding to alert")
					continue
				}
				// TODO: reconsider using block number for signing because alerts don't have block numbers for now
				if err := aas.cfg.AlertSender.SignAlertAndNotify(
					rt, alert, result.Request.Event.Network.ChainId, "", result.Timestamps,
				); err != nil {
					log.WithError(err).Panic("failed sign alert and notify")
				}
			}
			aas.publishMetrics(result)

			aas.lastOutputActivity.Set()
		}
	}()

	// Gear 1: loops over alerts and distributes to all agents
	go func() {
		// for each alert
		for alert := range aas.cfg.AlertChannel {
			// convert to message
			alertEvt, err := alert.ToMessage()
			if err != nil {
				log.WithError(err).Error("error converting alert event to message (skipping)")
				continue
			}

			// create a request
			requestId := uuid.Must(uuid.NewUUID())
			request := &protocol.EvaluateAlertRequest{RequestId: requestId.String(), Event: alertEvt}

			// forward to the pool
			aas.cfg.AgentPool.SendEvaluateAlertRequest(request)

			aas.lastInputActivity.Set()
		}
	}()

	return nil
}

func (aas *AlertAnalyzerService) Stop() error {
	return nil
}

func (aas *AlertAnalyzerService) Name() string {
	return "alert-analyzer"
}

// Health implements the health.Reporter interface.
func (aas *AlertAnalyzerService) Health() health.Reports {
	return health.Reports{
		aas.lastInputActivity.GetReport("event.input.time"),
		aas.lastOutputActivity.GetReport("event.output.time"),
	}
}

func NewAlertAnalyzerService(ctx context.Context, cfg AlertAnalyzerServiceConfig) (*AlertAnalyzerService, error) {
	return &AlertAnalyzerService{
		cfg: cfg,
		ctx: ctx,
	}, nil
}
