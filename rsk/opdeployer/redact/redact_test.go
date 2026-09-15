package redact

import (
	"testing"
)

func TestCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no secrets",
			args: []string{"forge", "script", "Deploy.s.sol", "--rpc-url", "http://localhost:8545"},
			want: "forge script Deploy.s.sol --rpc-url http://localhost:8545",
		},
		{
			name: "private-key separate arg",
			args: []string{"forge", "script", "--private-key", "0xdeadbeef1234567890", "--broadcast"},
			want: "forge script --private-key [REDACTED] --broadcast",
		},
		{
			name: "private-key=value combined",
			args: []string{"cast", "send", "--private-key=0xdeadbeef1234567890", "--legacy"},
			want: "cast send --private-key=[REDACTED] --legacy",
		},
		{
			name: "funder-private-key",
			args: []string{"cast", "send", "--funder-private-key=0xabc123", "--legacy"},
			want: "cast send --funder-private-key=[REDACTED] --legacy",
		},
		{
			name: "batcher-private-key separate",
			args: []string{"op-batcher", "--batcher-private-key", "0xsecretkey"},
			want: "op-batcher --batcher-private-key [REDACTED]",
		},
		{
			name: "proposer-private-key",
			args: []string{"op-proposer", "--proposer-private-key", "0xsecret"},
			want: "op-proposer --proposer-private-key [REDACTED]",
		},
		{
			name: "jwt-secret combined",
			args: []string{"op-geth", "--jwt-secret=./jwt.txt"},
			want: "op-geth --jwt-secret=[REDACTED]",
		},
		{
			name: "mnemonic",
			args: []string{"tool", "--mnemonic", "abandon abandon abandon"},
			want: "tool --mnemonic [REDACTED]",
		},
		{
			name: "multiple secrets",
			args: []string{"forge", "--private-key", "0xkey1", "--rpc-url", "http://host", "--funder-private-key", "0xkey2"},
			want: "forge --private-key [REDACTED] --rpc-url http://host --funder-private-key [REDACTED]",
		},
		{
			name: "private-key is last arg",
			args: []string{"tool", "--private-key"},
			want: "tool --private-key",
		},
		{
			name: "empty args",
			args: []string{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Command(tt.args)
			if got != tt.want {
				t.Errorf("Command() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "no userinfo",
			raw:  "http://localhost:8545",
			want: "http://localhost:8545",
		},
		{
			name: "with userinfo",
			raw:  "http://user:pass@host:8545",
			want: "http://***:***@host:8545",
		},
		{
			name: "user only no password",
			raw:  "http://user@host:8545",
			want: "http://***:***@host:8545",
		},
		{
			name: "https with path",
			raw:  "https://apikey:secret@rpc.example.com/v1/chain",
			want: "https://***:***@rpc.example.com/v1/chain",
		},
		{
			name: "not a URL",
			raw:  "not-a-url",
			want: "not-a-url",
		},
		{
			name: "empty string",
			raw:  "",
			want: "",
		},
		{
			name: "ws scheme with auth",
			raw:  "ws://admin:password@localhost:8546",
			want: "ws://***:***@localhost:8546",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := URL(tt.raw)
			if got != tt.want {
				t.Errorf("URL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestOutput(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want string
	}{
		{
			name: "no hex",
			s:    "Error: transaction failed with status 1",
			want: "Error: transaction failed with status 1",
		},
		{
			name: "contains 64-char hex key",
			s:    "Error using key d10f0f2609f060955fe5d38edf88010abd4b983374ce4f898caf3bb7485db1e7 failed",
			want: "Error using key d10f***b1e7 failed",
		},
		{
			name: "contains 0x-prefixed key",
			s:    "sent with 0xd10f0f2609f060955fe5d38edf88010abd4b983374ce4f898caf3bb7485db1e7 ok",
			want: "sent with 0xd10f***b1e7 ok",
		},
		{
			name: "short hex not redacted",
			s:    "tx hash: 0xabcdef1234567890",
			want: "tx hash: 0xabcdef1234567890",
		},
		{
			name: "empty string",
			s:    "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Output(tt.s)
			if got != tt.want {
				t.Errorf("Output(%q) = %q, want %q", tt.s, got, tt.want)
			}
		})
	}
}

func TestKey(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want string
	}{
		{
			name: "empty",
			s:    "",
			want: "",
		},
		{
			name: "short key",
			s:    "abcd1234",
			want: "[REDACTED]",
		},
		{
			name: "very short",
			s:    "abc",
			want: "[REDACTED]",
		},
		{
			name: "normal hex key without 0x",
			s:    "d10f0f2609f060955fe5d38edf88010abd4b983374ce4f898caf3bb7485db1e7",
			want: "d10f***b1e7",
		},
		{
			name: "normal hex key with 0x",
			s:    "0xd10f0f2609f060955fe5d38edf88010abd4b983374ce4f898caf3bb7485db1e7",
			want: "0xd10f***b1e7",
		},
		{
			name: "9 chars no prefix",
			s:    "abcdefghi",
			want: "abcd***fghi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Key(tt.s)
			if got != tt.want {
				t.Errorf("Key(%q) = %q, want %q", tt.s, got, tt.want)
			}
		})
	}
}
