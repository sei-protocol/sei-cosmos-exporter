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

// stubRPC serves a token with 6 decimals and a 123.456789 balance for expectWallet.
// associated is the sei_getEVMAddress answer; "" means the wallet is unassociated.
func stubRPC(t *testing.T, associated, expectWallet string) *httptest.Server {
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
				result = "0x" + leftPadHex(6)
			case strings.HasPrefix(data, selectorSymbol):
				result = abiString("USDC")
			case strings.HasPrefix(data, selectorBalanceOf):
				if !strings.HasSuffix(data, strings.TrimPrefix(expectWallet, "0x")) {
					t.Fatalf("balanceOf called for unexpected wallet: %s", data)
				}
				result = "0x" + leftPadHex(123_456_789) // 123.456789 USDC
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
			tokenMetadataCache = sync.Map{}
			server := stubRPC(t, tc.associated, tc.evmAddress)
			defer server.Close()

			req := httptest.NewRequest(http.MethodGet, "/metrics/evm-wallet?address="+testSeiWallet+"&tokens="+testToken, nil)
			rec := httptest.NewRecorder()
			EVMWalletHandler(rec, req, newEVMRPCClient(server.URL))

			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			want := `sei_chain_cosmos_wallet_erc20_balance{address="` + testSeiWallet + `",chain_id="test-1",evm_address="` + tc.evmAddress + `",symbol="USDC",token="` + testToken + `"} 123.456789`
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("missing %q in:\n%s", want, rec.Body.String())
			}
		})
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
