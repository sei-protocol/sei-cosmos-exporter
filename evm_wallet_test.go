package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	// sei1 encoding of the 20 bytes 0x11...11, so its cast address is testWallet.
	testSeiWallet = "sei1zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3v3x55w"
	testWallet    = "0x1111111111111111111111111111111111111111"
	testAssoc     = "0x3333333333333333333333333333333333333333"
	testToken     = "0x2222222222222222222222222222222222222222"
)

// abiString encodes s as a single ABI dynamic string return value.
func abiString(s string) string {
	padded := []byte(s)
	for len(padded)%32 != 0 {
		padded = append(padded, 0)
	}
	return "0x" + leftPadHex(32) + leftPadHex(uint64(len(s))) + hexOf(padded)
}

func leftPadHex(v uint64) string {
	h := strings.ToLower(strings.TrimLeft(hexOf([]byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	}), "0"))
	return strings.Repeat("0", 64-len(h)) + h
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// stubToken is the ERC-20 the stub RPC serves: symbolHex and balanceHex are
// returned verbatim, decimals is re-read on each call.
type stubToken struct {
	decimals   uint64
	symbolHex  string
	balanceHex string
}

// stubUnavailable as symbolHex makes the stub fail the symbol() request outright.
const stubUnavailable = "unavailable"

func usdc() *stubToken {
	return &stubToken{decimals: 6, symbolHex: abiString("USDC"), balanceHex: "0x" + leftPadHex(123_456_789)}
}

// stubRPC serves token for expectWallet. associated is the sei_getEVMAddress
// answer; "" means the wallet is unassociated.
func stubRPC(t *testing.T, associated, expectWallet string, token *stubToken) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad request body: %v", err)
		}

		var result string
		switch req.Method {
		case "sei_getEVMAddress":
			if associated == "" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 1,
					"error": map[string]interface{}{"code": -32000, "message": "failed to find EVM address for " + testSeiWallet}})
				return
			}
			result = associated
		case "eth_call":
			var call map[string]string
			_ = json.Unmarshal(req.Params[0], &call)
			data := strings.TrimPrefix(call["data"], "0x")
			switch {
			case strings.HasPrefix(data, selectorDecimals):
				result = "0x" + leftPadHex(token.decimals)
			case strings.HasPrefix(data, selectorSymbol):
				if token.symbolHex == stubUnavailable {
					http.Error(w, "upstream unavailable", http.StatusBadGateway)
					return
				}
				result = token.symbolHex
			case strings.HasPrefix(data, selectorBalanceOf):
				if !strings.HasSuffix(data, strings.TrimPrefix(expectWallet, "0x")) {
					t.Fatalf("balanceOf called for unexpected wallet: %s", data)
				}
				result = token.balanceHex
			default:
				t.Fatalf("unexpected eth_call data: %s", data)
			}
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
}

func TestEVMWalletHandler(t *testing.T) {
	ConstLabels = map[string]string{"chain_id": "test-1"}
	sdk.GetConfig().SetBech32PrefixForAccount("sei", "seipub")

	for name, tc := range map[string]struct{ associated, evmAddress string }{
		"unassociated wallet uses the cast address": {"", testWallet},
		"associated wallet uses the associated one": {testAssoc, testAssoc},
	} {
		t.Run(name, func(t *testing.T) {
			tokenSymbolCache = sync.Map{}
			server := stubRPC(t, tc.associated, tc.evmAddress, usdc())
			defer server.Close()

			body := scrape(t, server.URL, testToken)
			want := `sei_chain_cosmos_wallet_erc20_balance{address="` + testSeiWallet + `",chain_id="test-1",evm_address="` + tc.evmAddress + `",symbol="USDC",token="` + testToken + `"} 123.456789`
			if !strings.Contains(body, want) {
				t.Errorf("missing %q in:\n%s", want, body)
			}
		})
	}
}

func scrape(t *testing.T, rpcURL, tokens string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics/evm-wallet?address="+testSeiWallet+"&tokens="+tokens, nil)
	rec := httptest.NewRecorder()
	EVMWalletHandler(rec, req, newEVMRPCClient(rpcURL))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestEVMWalletHandlerSkipsAnEmptyBalance(t *testing.T) {
	ConstLabels = map[string]string{"chain_id": "test-1"}
	sdk.GetConfig().SetBech32PrefixForAccount("sei", "seipub")
	tokenSymbolCache = sync.Map{}
	token := usdc()
	token.balanceHex = "0x"
	server := stubRPC(t, "", testWallet, token)
	defer server.Close()

	if body := scrape(t, server.URL, testToken); strings.Contains(body, "sei_chain_cosmos_wallet_erc20_balance{") {
		t.Errorf("an empty balanceOf must not be reported as zero:\n%s", body)
	}
}

func TestEVMWalletHandlerReportsATokenWithoutASymbol(t *testing.T) {
	ConstLabels = map[string]string{"chain_id": "test-1"}
	sdk.GetConfig().SetBech32PrefixForAccount("sei", "seipub")
	tokenSymbolCache = sync.Map{}
	token := usdc()
	token.symbolHex = "0x"
	server := stubRPC(t, "", testWallet, token)
	defer server.Close()

	body := scrape(t, server.URL, testToken)
	if !strings.Contains(body, `symbol="",token="`+testToken+`"} 123.456789`) {
		t.Errorf("the balance must be reported with an empty symbol:\n%s", body)
	}
}

func TestEVMWalletHandlerDoesNotPinASymbolItCouldNotRead(t *testing.T) {
	ConstLabels = map[string]string{"chain_id": "test-1"}
	sdk.GetConfig().SetBech32PrefixForAccount("sei", "seipub")
	tokenSymbolCache = sync.Map{}
	token := usdc()
	token.symbolHex = stubUnavailable
	server := stubRPC(t, "", testWallet, token)
	defer server.Close()

	if body := scrape(t, server.URL, testToken); strings.Contains(body, "sei_chain_cosmos_wallet_erc20_balance{") {
		t.Errorf("a balance must not be reported under a symbol that could not be read:\n%s", body)
	}
	token.symbolHex = abiString("USDC")
	if body := scrape(t, server.URL, testToken); !strings.Contains(body, `symbol="USDC",token="`+testToken+`"} 123.456789`) {
		t.Errorf("the symbol must be read again on the next scrape:\n%s", body)
	}
}

func TestEVMWalletHandlerFollowsDecimalsButPinsTheSymbol(t *testing.T) {
	ConstLabels = map[string]string{"chain_id": "test-1"}
	sdk.GetConfig().SetBech32PrefixForAccount("sei", "seipub")
	tokenSymbolCache = sync.Map{}
	token := usdc()
	server := stubRPC(t, "", testWallet, token)
	defer server.Close()

	scrape(t, server.URL, testToken)
	token.decimals = 3
	token.symbolHex = abiString("USDC.n")
	body := scrape(t, server.URL, testToken)
	if !strings.Contains(body, `symbol="USDC",token="`+testToken+`"} 123456.789`) {
		t.Errorf("the scale must follow decimals and the symbol must not:\n%s", body)
	}
}

func TestEVMWalletHandlerBoundsTheTokenList(t *testing.T) {
	sdk.GetConfig().SetBech32PrefixForAccount("sei", "seipub")
	tokens := make([]string, maxTokensPerRequest+1)
	for i := range tokens {
		tokens[i] = testToken
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics/evm-wallet?address="+testSeiWallet+"&tokens="+strings.Join(tokens, ","), nil)
	rec := httptest.NewRecorder()
	EVMWalletHandler(rec, req, newEVMRPCClient("http://127.0.0.1:0"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestSanitizeSymbol(t *testing.T) {
	got := sanitizeSymbol("US\x00DC \n" + strings.Repeat("x", 40))
	if want := "USDC" + strings.Repeat("x", maxSymbolLen-4); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEVMWalletHandlerRejectsHexAddress(t *testing.T) {
	sdk.GetConfig().SetBech32PrefixForAccount("sei", "seipub")
	req := httptest.NewRequest(http.MethodGet, "/metrics/evm-wallet?address="+testWallet, nil)
	rec := httptest.NewRecorder()
	EVMWalletHandler(rec, req, newEVMRPCClient("http://127.0.0.1:0"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestDecodeABIString(t *testing.T) {
	got, err := decodeABIString(abiString("WSEI"))
	if err != nil || got != "WSEI" {
		t.Fatalf("got %q, %v", got, err)
	}
	bytes32 := "0x" + hexOf([]byte("MKR")) + strings.Repeat("00", 29)
	got, err = decodeABIString(bytes32)
	if err != nil || got != "MKR" {
		t.Fatalf("bytes32 symbol: got %q, %v", got, err)
	}
}
