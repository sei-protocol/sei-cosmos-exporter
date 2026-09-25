package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ERC-20 function selectors: keccak256 of the canonical signature, first 4 bytes.
const (
	selectorBalanceOf = "70a08231"
	selectorDecimals  = "313ce567"
	selectorSymbol    = "95d89b41"
)

var evmAddressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

type tokenMetadata struct {
	symbol      string
	coefficient float64
}

// tokenMetadataCache holds symbol/decimals per token contract. Neither changes
// after deployment, so they are fetched once per process.
var tokenMetadataCache sync.Map

type evmRPCClient struct {
	url  string
	http *http.Client
}

func newEVMRPCClient(url string) *evmRPCClient {
	return &evmRPCClient{url: url, http: &http.Client{Timeout: 10 * time.Second}}
}

func (c *evmRPCClient) call(method string, params ...interface{}) (string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return "", err
	}

	resp, err := c.http.Post(c.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var decoded struct {
		Result string `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", err
	}
	if decoded.Error != nil {
		return "", fmt.Errorf("%s: rpc error %d: %s", method, decoded.Error.Code, decoded.Error.Message)
	}
	return decoded.Result, nil
}

func (c *evmRPCClient) ethCall(to, data string) (string, error) {
	return c.call("eth_call", map[string]string{"to": to, "data": data}, "latest")
}

func hexToBig(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return big.NewInt(0), nil
	}
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return nil, fmt.Errorf("not a hex quantity: %q", s)
	}
	return v, nil
}

func bigToFloat(v *big.Int) float64 {
	f, _ := new(big.Float).SetInt(v).Float64()
	return f
}

func abiAddressArg(address string) string {
	return strings.Repeat("0", 24) + strings.ToLower(strings.TrimPrefix(address, "0x"))
}

// decodeABIString decodes a single ABI-encoded dynamic `string` return value.
// Tokens that return `bytes32` for symbol() are handled by trimming zero bytes.
func decodeABIString(result string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(result, "0x"))
	if err != nil {
		return "", err
	}
	if len(raw) == 32 {
		return string(bytes.TrimRight(raw, "\x00")), nil
	}
	if len(raw) < 64 {
		return "", fmt.Errorf("abi string too short: %d bytes", len(raw))
	}
	offset := new(big.Int).SetBytes(raw[:32]).Uint64()
	if offset+32 > uint64(len(raw)) {
		return "", fmt.Errorf("abi string offset out of range")
	}
	length := new(big.Int).SetBytes(raw[offset : offset+32]).Uint64()
	if offset+32+length > uint64(len(raw)) {
		return "", fmt.Errorf("abi string length out of range")
	}
	return string(raw[offset+32 : offset+32+length]), nil
}

func (c *evmRPCClient) tokenMetadata(token string) (tokenMetadata, error) {
	if cached, ok := tokenMetadataCache.Load(token); ok {
		return cached.(tokenMetadata), nil
	}

	decimalsHex, err := c.ethCall(token, "0x"+selectorDecimals)
	if err != nil {
		return tokenMetadata{}, err
	}
	decimals, err := hexToBig(decimalsHex)
	if err != nil {
		return tokenMetadata{}, err
	}

	symbolHex, err := c.ethCall(token, "0x"+selectorSymbol)
	if err != nil {
		return tokenMetadata{}, err
	}
	symbol, err := decodeABIString(symbolHex)
	if err != nil {
		return tokenMetadata{}, err
	}

	meta := tokenMetadata{symbol: symbol, coefficient: math.Pow10(int(decimals.Int64()))}
	tokenMetadataCache.Store(token, meta)
	return meta, nil
}

func (c *evmRPCClient) tokenBalance(token, wallet string) (*big.Int, error) {
	result, err := c.ethCall(token, "0x"+selectorBalanceOf+abiAddressArg(wallet))
	if err != nil {
		return nil, err
	}
	return hexToBig(result)
}

func (c *evmRPCClient) nativeBalance(wallet string) (*big.Int, error) {
	result, err := c.call("eth_getBalance", wallet, "latest")
	if err != nil {
		return nil, err
	}
	return hexToBig(result)
}

// EVMWalletHandler serves /metrics/evm-wallet?address=0x...&tokens=0x...,0x...
// It exposes the wallet's native balance and its balance of each listed ERC-20
// contract, both read over EVM JSON-RPC.
func EVMWalletHandler(w http.ResponseWriter, r *http.Request, rpc *evmRPCClient) {
	requestStart := time.Now()

	sublogger := log.With().
		Str("request-id", uuid.New().String()).
		Logger()

	address := r.URL.Query().Get("address")
	if !evmAddressPattern.MatchString(address) {
		sublogger.Error().Str("address", address).Msg("Not a 0x address")
		http.Error(w, "address must be a 0x-prefixed 20-byte hex address", http.StatusBadRequest)
		return
	}
	address = strings.ToLower(address)

	var tokens []string
	for _, token := range strings.Split(r.URL.Query().Get("tokens"), ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if !evmAddressPattern.MatchString(token) {
			sublogger.Error().Str("token", token).Msg("Not a 0x token address")
			http.Error(w, "tokens must be comma-separated 0x-prefixed 20-byte hex addresses", http.StatusBadRequest)
			return
		}
		tokens = append(tokens, strings.ToLower(token))
	}

	nativeBalanceGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "evm_wallet_balance",
			Help:        "Native balance of the wallet as seen from the EVM, in display denom",
			ConstLabels: ConstLabels,
		},
		[]string{"address", "denom"},
	)

	tokenBalanceGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "evm_wallet_token_balance",
			Help:        "ERC-20 balance of the wallet, scaled by the token's decimals()",
			ConstLabels: ConstLabels,
		},
		[]string{"address", "token", "symbol"},
	)

	tokenErrorGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "evm_wallet_token_query_failed",
			Help:        "1 when the ERC-20 balance for this wallet/token pair could not be read this scrape",
			ConstLabels: ConstLabels,
		},
		[]string{"address", "token"},
	)

	registry := prometheus.NewRegistry()
	registry.MustRegister(nativeBalanceGauge)
	registry.MustRegister(tokenBalanceGauge)
	registry.MustRegister(tokenErrorGauge)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		queryStart := time.Now()

		wei, err := rpc.nativeBalance(address)
		if err != nil {
			sublogger.Error().Str("address", address).Err(err).Msg("Could not get native balance")
			return
		}

		sublogger.Debug().
			Str("address", address).
			Float64("request-time", time.Since(queryStart).Seconds()).
			Msg("Finished querying native balance")

		// eth_getBalance is denominated in 18-decimal wei regardless of the chain's base denom.
		nativeBalanceGauge.With(prometheus.Labels{
			"address": address,
			"denom":   Denom,
		}).Set(bigToFloat(wei) / 1e18)
	}()

	for _, token := range tokens {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			queryStart := time.Now()

			meta, err := rpc.tokenMetadata(token)
			if err != nil {
				sublogger.Error().Str("token", token).Err(err).Msg("Could not get token metadata")
				tokenErrorGauge.With(prometheus.Labels{"address": address, "token": token}).Set(1)
				return
			}

			balance, err := rpc.tokenBalance(token, address)
			if err != nil {
				sublogger.Error().Str("address", address).Str("token", token).Err(err).Msg("Could not get token balance")
				tokenErrorGauge.With(prometheus.Labels{"address": address, "token": token}).Set(1)
				return
			}

			sublogger.Debug().
				Str("address", address).
				Str("token", token).
				Float64("request-time", time.Since(queryStart).Seconds()).
				Msg("Finished querying token balance")

			tokenErrorGauge.With(prometheus.Labels{"address": address, "token": token}).Set(0)
			tokenBalanceGauge.With(prometheus.Labels{
				"address": address,
				"token":   token,
				"symbol":  meta.symbol,
			}).Set(bigToFloat(balance) / meta.coefficient)
		}(token)
	}

	wg.Wait()

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
	sublogger.Info().
		Str("method", "GET").
		Str("endpoint", "/metrics/evm-wallet?address="+address).
		Float64("request-time", time.Since(requestStart).Seconds()).
		Msg("Request processed")
}
