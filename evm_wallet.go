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

	sdk "github.com/cosmos/cosmos-sdk/types"
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

// evmAddress resolves a sei1 address to the 0x address the EVM sees it as: the
// associated address when one exists, otherwise the cast of the same 20 bytes,
// matching the chain's GetEVMAddressOrDefault.
func (c *evmRPCClient) evmAddress(acc sdk.AccAddress) (string, error) {
	result, err := c.call("sei_getEVMAddress", acc.String())
	if err == nil {
		if !evmAddressPattern.MatchString(result) {
			return "", fmt.Errorf("sei_getEVMAddress returned %q", result)
		}
		return strings.ToLower(result), nil
	}
	if !strings.Contains(err.Error(), "failed to find EVM address") {
		return "", err
	}
	return "0x" + hex.EncodeToString(acc), nil
}

// EVMWalletHandler serves /metrics/evm-wallet?address=sei1...&tokens=0x...,0x...
// It exposes the wallet's balance of each listed ERC-20 contract, read over EVM
// JSON-RPC at the wallet's EVM address, under the same metric name and labels
// seid's cosmosmetrics reports so the two sources are interchangeable.
func EVMWalletHandler(w http.ResponseWriter, r *http.Request, rpc *evmRPCClient) {
	requestStart := time.Now()

	sublogger := log.With().
		Str("request-id", uuid.New().String()).
		Logger()

	address := r.URL.Query().Get("address")
	acc, err := sdk.AccAddressFromBech32(address)
	if err != nil {
		sublogger.Error().Str("address", address).Err(err).Msg("Not a bech32 account address")
		http.Error(w, "address must be a bech32 account address", http.StatusBadRequest)
		return
	}

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

	tokenBalanceGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sei_chain_cosmos_wallet_erc20_balance",
			Help:        "ERC-20 balance of the wallet's EVM address by token, in token units",
			ConstLabels: ConstLabels,
		},
		[]string{"address", "evm_address", "token", "symbol"},
	)

	registry := prometheus.NewRegistry()
	registry.MustRegister(tokenBalanceGauge)

	evmAddress, err := rpc.evmAddress(acc)
	if err != nil {
		sublogger.Error().Str("address", address).Err(err).Msg("Could not resolve EVM address")
		http.Error(w, "could not resolve EVM address: "+err.Error(), http.StatusBadGateway)
		return
	}

	var wg sync.WaitGroup
	for _, token := range tokens {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			queryStart := time.Now()

			meta, err := rpc.tokenMetadata(token)
			if err != nil {
				sublogger.Error().Str("token", token).Err(err).Msg("Could not get token metadata")
				return
			}

			balance, err := rpc.tokenBalance(token, evmAddress)
			if err != nil {
				sublogger.Error().Str("address", address).Str("token", token).Err(err).Msg("Could not get token balance")
				return
			}

			sublogger.Debug().
				Str("address", address).
				Str("token", token).
				Float64("request-time", time.Since(queryStart).Seconds()).
				Msg("Finished querying token balance")

			tokenBalanceGauge.With(prometheus.Labels{
				"address":     address,
				"evm_address": evmAddress,
				"token":       token,
				"symbol":      meta.symbol,
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
