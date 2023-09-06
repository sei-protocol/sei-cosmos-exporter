package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	tmrpcclient "github.com/tendermint/tendermint/rpc/client"
	tmrpceventstream "github.com/tendermint/tendermint/rpc/client/eventstream"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"github.com/tendermint/tendermint/rpc/coretypes"
	tmjsonclient "github.com/tendermint/tendermint/rpc/jsonrpc/client"
	tmtypes "github.com/tendermint/tendermint/types"
)

var (
	queryEventBankTransfer = fmt.Sprintf(
		"%s='%s' AND %s='%s'",
		tmtypes.EventTypeKey,
		tmtypes.EventTxValue,
		"message.action",
		"/cosmos.bank.v1beta1.MsgSend",
	)
)

type EventCollector struct {
	rpcClient             tmrpcclient.Client
	logger                zerolog.Logger
	gauge                 *prometheus.GaugeVec
	BankTransferThreshold float64
}

func NewEventCollector(tmRPC string, logger zerolog.Logger, bankTransferThreshold float64) (*EventCollector, error) {
	httpClient, err := tmjsonclient.DefaultHTTPClient(tmRPC)
	if err != nil {
		return nil, err
	}
	// no timeout because we will continue fetching events continuously
	rpcClient, err := rpchttp.NewWithClient(tmRPC, httpClient)
	if err != nil {
		return nil, err
	}
	transfersValueGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "cosmos_bank_transfer_amount",
			Help:        "Number of tokens transferred in a transfer message",
			ConstLabels: ConstLabels,
		},
		[]string{"denom", "sender", "recipient"},
	)
	return &EventCollector{
		rpcClient:             rpcClient,
		logger:                logger,
		gauge:                 transfersValueGauge,
		BankTransferThreshold: bankTransferThreshold,
	}, nil
}

func (s EventCollector) Start(
	ctx context.Context,
) error {
	if err := s.rpcClient.Start(ctx); err != nil {
		return err
	}
	s.logger.Info().Msg("Starting StreamCollector")
	go s.RunBankTransferEventStream(ctx)

	return nil
}

func (s EventCollector) RunBankTransferEventStream(ctx context.Context) {
	eventStream := tmrpceventstream.New(s.rpcClient, queryEventBankTransfer, &tmrpceventstream.StreamOptions{
		WaitTime: 300 * time.Millisecond,
	})
	streamEventErr := make(chan error, 1)
	go func() {
		streamEventErr <- eventStream.Run(ctx, s.HandleBankTransferEvent)
	}()
	for {
		// try recovering forever if only missed items errors
		err := <-streamEventErr
		if _, ok := err.(*tmrpceventstream.MissedItemsError); ok {
			//fallen behind, restart
			s.logger.Err(err).Msg("Error in eventstream")
			// reset + run again
			eventStream.Reset()
			go func() {
				streamEventErr <- eventStream.Run(ctx, s.HandleBankTransferEvent)
			}()
		} else {
			panic(err) // panic so we trigger `up` alerting
		}
	}
}

func (s EventCollector) HandleBankTransferEvent(eventItem *coretypes.EventItem) error {
	eventData, err := tmtypes.TryUnmarshalEventData(eventItem.Data)
	if err != nil {
		s.logger.Err(err).Msg("Failed to unmarshal event data")
		return nil
	}
	eventDataTx, ok := eventData.(tmtypes.EventDataTx)
	if !ok {
		s.logger.Err(err).Msg("Failed to parse event from eventDataNewBlockHeader")
		return nil
	} else {
		events := eventDataTx.Result.Events
		for _, event := range events {
			if event.Type == banktypes.EventTypeTransfer {
				// check the balance
				var amount float64
				var denom string
				var sender string
				var recipient string
				for _, attr := range event.Attributes {
					attrKey := string(attr.Key)
					switch attrKey {
					case sdk.AttributeKeyAmount:
						amountStr := string(attr.Value)
						// separate the denom
						re := regexp.MustCompile(`\d+|\D+`)
						res := re.FindAllString(amountStr, -1)

						denom = res[1]
						amount, err = strconv.ParseFloat(res[0], 64)
						if err != nil {
							s.logger.Err(err).Msg("")
						}
					case banktypes.AttributeKeyRecipient:
						recipient = string(attr.Value)
					case banktypes.AttributeKeySender:
						sender = string(attr.Value)
					}
				}
				if amount > s.BankTransferThreshold {
					s.gauge.With(prometheus.Labels{
						"denom":     denom,
						"sender":    sender,
						"recipient": recipient,
					}).Set(amount)
					// Expire the metrics after 5 minutes
					go func() {
						time.Sleep(5 * time.Minute)
						s.gauge.Delete(prometheus.Labels{
							"denom":     denom,
							"sender":    sender,
							"recipient": recipient,
						})
					}()
				}
			}
		}
	}
	return nil
}

func (s EventCollector) StreamHandler(w http.ResponseWriter, r *http.Request) {
	requestStart := time.Now()

	sublogger := log.With().
		Str("request-id", uuid.New().String()).
		Logger()

	registry := prometheus.NewRegistry()
	registry.MustRegister(s.gauge)

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
	sublogger.Info().
		Str("method", "GET").
		Str("endpoint", "/metrics/event").
		Float64("request-time", time.Since(requestStart).Seconds()).
		Msg("Request processed")
}
