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
	queryInterval = 100 * time.Millisecond
)

type StreamCollector struct {
	rpcClient          tmrpcclient.Client
	logger             zerolog.Logger
	recentlySeenHashes *simplelru.LRU
	histogram          *prometheus.HistogramVec
}

func NewStreamCollector(tmRPC string, logger zerolog.Logger) (*StreamCollector, error) {
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
	transfersValueHistogram := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cosmos_bank_transfer_amount",
			Help:    "Number of tokens transferred in a transfer message",
			Buckets: prometheus.ExponentialBuckets(1e6, 10, 10),
		},
		[]string{"denom"},
	)
	return &StreamCollector{
		rpcClient:          rpcClient,
		logger:             logger,
		recentlySeenHashes: recentlySeen,
		histogram:          transfersValueHistogram,
	}, nil
}

func (s StreamCollector) Start(
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

func (s StreamCollector) subscribe(
	ctx context.Context,
	eventsClient tmrpcclient.EventsClient,
) {
	s.logger.Info().Msg(fmt.Sprintf("Listening for bank transfer event with query: %s\n", queryEventBankTransfer))
	for {
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
					for _, attr := range event.Attributes {
						if string(attr.Key) == sdk.AttributeKeyAmount {
							amountStr := string(attr.Value)
							// separate the denom
							re := regexp.MustCompile(`\d+|\D+`)
							res := re.FindAllString(amountStr, -1)

							denom = res[1]
							amount, err = strconv.ParseFloat(res[0], 64)
							if err != nil {
								s.logger.Err(err).Msg("")
							}
							break
						}
					}
					if amount != 0 {
						s.histogram.With(prometheus.Labels{"denom": denom}).Observe(amount)
					}
				}
			}
		}

		time.Sleep(queryInterval)
	}
}

func (s StreamCollector) StreamHandler(w http.ResponseWriter, r *http.Request) {
	requestStart := time.Now()

	sublogger := log.With().
		Str("request-id", uuid.New().String()).
		Logger()

	registry := prometheus.NewRegistry()
	registry.MustRegister(s.histogram)

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
	sublogger.Info().
		Str("method", "GET").
		Str("endpoint", "/metrics/stream").
		Float64("request-time", time.Since(requestStart).Seconds()).
		Msg("Request processed")
}
