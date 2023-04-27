package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/google/uuid"
	"github.com/hashicorp/golang-lru/simplelru"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	tmrpcclient "github.com/tendermint/tendermint/rpc/client"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	tmjsonclient "github.com/tendermint/tendermint/rpc/jsonrpc/client"
	tmtypes "github.com/tendermint/tendermint/types"
)

var (
	started                = false
	queryEventBankTransfer = fmt.Sprintf(
		"%s='%s' AND %s='%s'",
		tmtypes.EventTypeKey,
		tmtypes.EventTxValue,
		"message.action",
		"/cosmos.bank.v1beta1.MsgSend",
	)
	queryInterval = 300 * time.Millisecond
)

type EventCollector struct {
	rpcClient          tmrpcclient.Client
	logger             zerolog.Logger
	recentlySeenHashes *simplelru.LRU
	counter            *prometheus.CounterVec
}

func NewEventCollector(tmRPC string, logger zerolog.Logger) (*EventCollector, error) {
	httpClient, err := tmjsonclient.DefaultHTTPClient(tmRPC)
	if err != nil {
		return nil, err
	}

	httpClient.Timeout = 500 * time.Millisecond

	rpcClient, err := rpchttp.NewWithClient(tmRPC, httpClient)
	if err != nil {
		return nil, err
	}
	recentlySeen, err := simplelru.NewLRU(10, func(k interface{}, v interface{}) { /* noop */ })
	if err != nil {
		return nil, err
	}
	transfersValueCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "cosmos_bank_transfer_amount",
			Help:        "Number of tokens transferred in a transfer message",
			ConstLabels: ConstLabels,
		},
		[]string{"denom", "sender", "recipient"},
	)
	return &EventCollector{
		rpcClient:          rpcClient,
		logger:             logger,
		recentlySeenHashes: recentlySeen,
		counter:            transfersValueCounter,
	}, nil
}

func (s EventCollector) Start(
	ctx context.Context,
) error {
	if !started {
		if err := s.rpcClient.Start(ctx); err != nil {
			return err
		}
		s.logger.Info().Msg("Starting StreamCollector")
		go s.subscribe(ctx, s.rpcClient)
		started = true
	}
	return nil
}

func (s EventCollector) subscribe(
	ctx context.Context,
	eventsClient tmrpcclient.EventsClient,
) {
	s.logger.Info().Msg(fmt.Sprintf("Listening for bank transfer event with query: %s\n", queryEventBankTransfer))
	for {
		// TODO: this has issues with multiple events at the same time since it only gets latest (need to do custom one that will accept multiple / or go by height?)
		eventData, err := tmrpcclient.WaitForOneEvent(ctx, eventsClient, queryEventBankTransfer)
		if err != nil {
			s.logger.Debug().Msg("Failed to query EventTypeTransfer")
			continue
		}
		eventDataTx, ok := eventData.(tmtypes.EventDataTx)
		if !ok {
			s.logger.Err(err).Msg("Failed to parse event from eventDataNewBlockHeader")
			continue
		} else {
			result := eventDataTx.Result
			txHash := hex.EncodeToString(tmtypes.Tx(eventDataTx.Tx).Hash())
			events := result.Events

			// dedupe to avoid double counting txs
			if s.recentlySeenHashes.Contains(txHash) {
				continue
			}
			s.recentlySeenHashes.Add(txHash, struct{}{})

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
							// todo
						case banktypes.AttributeKeySender:
							sender = string(attr.Value)
						}
					}
					if amount > 1e11 {
						s.counter.With(prometheus.Labels{
							"denom":     denom,
							"sender":    sender,
							"recipient": recipient,
						}).Add(amount)
					}
				}
			}
		}

		time.Sleep(queryInterval)
	}
}

func (s EventCollector) StreamHandler(w http.ResponseWriter, r *http.Request) {
	requestStart := time.Now()

	sublogger := log.With().
		Str("request-id", uuid.New().String()).
		Logger()

	registry := prometheus.NewRegistry()
	registry.MustRegister(s.counter)

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
	sublogger.Info().
		Str("method", "GET").
		Str("endpoint", "/metrics/stream").
		Float64("request-time", time.Since(requestStart).Seconds()).
		Msg("Request processed")
}
