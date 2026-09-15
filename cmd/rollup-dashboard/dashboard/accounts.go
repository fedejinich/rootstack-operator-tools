package dashboard

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fedejinich/rootstack-operator-tools/rsk/opdeployer/keyderive"
)

// AccountInfo holds derived address and live balances for one role.
type AccountInfo struct {
	Role      string
	Address   common.Address
	L1Balance *big.Int
	L2Balance *big.Int
}

// AccountsPanel manages the accounts popover state.
type AccountsPanel struct {
	mu        sync.RWMutex
	accounts  []AccountInfo
	masterKey *ecdsa.PrivateKey

	Visible  bool
	selected int
	funding  bool
	fundBuf  string

	statusMsg string
	statusAt  time.Time
}

// NewAccountsPanel derives all role addresses from the master key.
// Returns nil if the master key is empty or invalid.
func NewAccountsPanel(masterKeyHex string) *AccountsPanel {
	if masterKeyHex == "" {
		return nil
	}

	masterAddr, err := keyderive.AddressFromKey(masterKeyHex)
	if err != nil {
		return nil
	}

	batcherKey, batcherAddr, err := keyderive.DeriveKey(masterKeyHex, keyderive.RoleBatcher)
	if err != nil {
		return nil
	}
	_ = batcherKey

	_, proposerAddr, err := keyderive.DeriveKey(masterKeyHex, keyderive.RoleProposer)
	if err != nil {
		return nil
	}

	raw := strings.TrimPrefix(masterKeyHex, "0x")
	keyBytes, err := crypto.HexToECDSA(raw)
	if err != nil {
		return nil
	}

	return &AccountsPanel{
		masterKey: keyBytes,
		accounts: []AccountInfo{
			{Role: "Master (EOA)", Address: masterAddr},
			{Role: "Batcher", Address: batcherAddr},
			{Role: "Proposer", Address: proposerAddr},
		},
	}
}

// PollBalances refreshes L1 and L2 balances for all accounts.
func (ap *AccountsPanel) PollBalances(ctx context.Context, l1RPC, l2RPC string) {
	if ap == nil {
		return
	}

	var l1Client, l2Client *ethclient.Client
	if l1RPC != "" {
		if c, err := ethclient.DialContext(ctx, l1RPC); err == nil {
			l1Client = c
			defer c.Close()
		}
	}
	if l2RPC != "" {
		if c, err := ethclient.DialContext(ctx, l2RPC); err == nil {
			l2Client = c
			defer c.Close()
		}
	}

	ap.mu.Lock()
	defer ap.mu.Unlock()

	for i := range ap.accounts {
		addr := ap.accounts[i].Address
		if l1Client != nil {
			if bal, err := l1Client.BalanceAt(ctx, addr, nil); err == nil {
				ap.accounts[i].L1Balance = bal
			}
		}
		if l2Client != nil {
			if bal, err := l2Client.BalanceAt(ctx, addr, nil); err == nil {
				ap.accounts[i].L2Balance = bal
			}
		}
	}
}

// Snapshot returns a copy of the account list for rendering.
func (ap *AccountsPanel) Snapshot() []AccountInfo {
	if ap == nil {
		return nil
	}
	ap.mu.RLock()
	defer ap.mu.RUnlock()
	out := make([]AccountInfo, len(ap.accounts))
	copy(out, ap.accounts)
	return out
}

// MoveUp moves the selection cursor up.
func (ap *AccountsPanel) MoveUp() {
	if ap.selected > 0 {
		ap.selected--
	}
}

// MoveDown moves the selection cursor down.
func (ap *AccountsPanel) MoveDown() {
	if ap.selected < len(ap.accounts)-1 {
		ap.selected++
	}
}

// StartFunding enters funding mode for the selected account.
func (ap *AccountsPanel) StartFunding() {
	ap.funding = true
	ap.fundBuf = "0.01"
}

// CancelFunding exits funding mode.
func (ap *AccountsPanel) CancelFunding() {
	ap.funding = false
	ap.fundBuf = ""
}

// IsFunding returns whether the panel is in funding input mode.
func (ap *AccountsPanel) IsFunding() bool {
	return ap.funding
}

// HandleFundingKey processes a keystroke during funding input.
// Returns true if the key was consumed.
func (ap *AccountsPanel) HandleFundingKey(key string) bool {
	switch key {
	case "backspace":
		if len(ap.fundBuf) > 0 {
			ap.fundBuf = ap.fundBuf[:len(ap.fundBuf)-1]
		}
		return true
	case "esc":
		ap.CancelFunding()
		return true
	default:
		if len(key) == 1 && ((key[0] >= '0' && key[0] <= '9') || key[0] == '.') {
			ap.fundBuf += key
			return true
		}
	}
	return false
}

// FundAmount returns the parsed fund amount and the target address.
func (ap *AccountsPanel) FundAmount() (addr common.Address, amountWei *big.Int, err error) {
	if ap.selected >= len(ap.accounts) {
		return common.Address{}, nil, fmt.Errorf("no account selected")
	}
	addr = ap.accounts[ap.selected].Address

	amountWei, err = parseEtherAmount(ap.fundBuf)
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("invalid amount %q: %w", ap.fundBuf, err)
	}

	return addr, amountWei, nil
}

// SelectedRole returns the role name of the currently selected account.
func (ap *AccountsPanel) SelectedRole() string {
	if ap.selected < len(ap.accounts) {
		return ap.accounts[ap.selected].Role
	}
	return ""
}

// SetStatus sets a temporary status message on the panel.
func (ap *AccountsPanel) SetStatus(msg string) {
	ap.statusMsg = msg
	ap.statusAt = time.Now()
}

// MasterKey returns the master ECDSA key for signing funding transactions.
func (ap *AccountsPanel) MasterKey() *ecdsa.PrivateKey {
	if ap == nil {
		return nil
	}
	return ap.masterKey
}

// parseEtherAmount converts a decimal string (e.g. "0.01") to wei.
func parseEtherAmount(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty amount")
	}

	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return nil, fmt.Errorf("invalid number")
	}

	wholePart := parts[0]
	if wholePart == "" {
		wholePart = "0"
	}
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}

	// Pad or truncate fractional part to 18 decimals
	if len(fracPart) > 18 {
		fracPart = fracPart[:18]
	}
	for len(fracPart) < 18 {
		fracPart += "0"
	}

	combined := wholePart + fracPart
	wei, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return nil, fmt.Errorf("invalid number %q", s)
	}
	return wei, nil
}

// formatEther converts wei to a human-readable ether string.
func formatEther(wei *big.Int) string {
	if wei == nil {
		return "—"
	}
	// wei / 1e18 with 6 decimal places
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	whole := new(big.Int).Div(wei, divisor)
	remainder := new(big.Int).Mod(wei, divisor)

	// 6 decimal places
	fracDivisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)
	frac := new(big.Int).Div(remainder, fracDivisor)

	return fmt.Sprintf("%d.%06d", whole, frac)
}

// shortenAddress returns 0x1234...abcd
func shortenAddress(addr common.Address) string {
	hex := addr.Hex()
	return hex[:6] + "..." + hex[len(hex)-4:]
}

// --- Funding ---

// SendFunding sends a native value transfer from the master key to the
// target address on L1. It returns the tx hash on success.
func SendFunding(ctx context.Context, l1RPC string, masterKey *ecdsa.PrivateKey, to common.Address, amountWei *big.Int) (common.Hash, error) {
	client, err := ethclient.DialContext(ctx, l1RPC)
	if err != nil {
		return common.Hash{}, fmt.Errorf("dial L1: %w", err)
	}
	defer client.Close()

	from := crypto.PubkeyToAddress(masterKey.PublicKey)

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, fmt.Errorf("get nonce: %w", err)
	}

	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("suggest gas price: %w", err)
	}

	gasLimit := uint64(21000)
	tx := types.NewTransaction(nonce, to, amountWei, gasLimit, gasPrice, nil)

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("get chain ID: %w", err)
	}

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), masterKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign tx: %w", err)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return common.Hash{}, fmt.Errorf("send tx: %w", err)
	}

	return signedTx.Hash(), nil
}

// --- Rendering ---

var (
	acctTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	acctLabel    = lipgloss.NewStyle().Foreground(lipgloss.Color("248"))
	acctValue    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	acctDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	acctSelected = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	acctInput    = lipgloss.NewStyle().Foreground(lipgloss.Color("117"))
)

// Render draws the accounts popover centered in the viewport.
func (ap *AccountsPanel) Render(width, height int) string {
	if ap == nil {
		return ""
	}

	accounts := ap.Snapshot()

	var lines []string
	lines = append(lines, acctTitle.Render("  Accounts & Balances"))
	lines = append(lines, acctTitle.Render("  "+strings.Repeat("═", 40)))
	lines = append(lines, "")

	header := fmt.Sprintf("  %-14s %-14s %16s %16s", "Role", "Address", "L1 Balance", "L2 Balance")
	lines = append(lines, acctDim.Render(header))
	lines = append(lines, acctDim.Render("  "+strings.Repeat("─", 62)))

	for i, a := range accounts {
		cursor := "  "
		roleStyle := acctLabel
		if i == ap.selected {
			cursor = acctSelected.Render("> ")
			roleStyle = acctSelected
		}

		line := fmt.Sprintf("%s%-14s %s %16s %16s",
			cursor,
			roleStyle.Render(a.Role),
			acctDim.Render(shortenAddress(a.Address)),
			acctValue.Render(formatEther(a.L1Balance)),
			acctValue.Render(formatEther(a.L2Balance)),
		)
		lines = append(lines, line)
	}

	lines = append(lines, "")

	if ap.funding {
		target := accounts[ap.selected]
		lines = append(lines, fmt.Sprintf("  Fund %s (%s) on L1:", acctSelected.Render(target.Role), acctDim.Render(shortenAddress(target.Address))))
		lines = append(lines, fmt.Sprintf("  Amount (RBTC): %s▌", acctInput.Render(ap.fundBuf)))
		lines = append(lines, acctDim.Render("  [Enter] send  [Esc] cancel"))
	} else {
		lines = append(lines, acctDim.Render("  [f] fund selected  [↑/↓] select  [Esc/q] close"))
	}

	if ap.statusMsg != "" && time.Since(ap.statusAt) < 10*time.Second {
		lines = append(lines, "")
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(ap.statusMsg))
	}

	content := strings.Join(lines, "\n")

	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		lipgloss.NewStyle().
			Padding(1, 2).
			Border(lipgloss.DoubleBorder()).
			BorderForeground(lipgloss.Color("99")).
			Render(content),
		lipgloss.WithWhitespaceChars(" "),
	)
}
