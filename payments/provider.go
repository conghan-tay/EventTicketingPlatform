package payments

import (
	"context"
	"errors"
	"sync"

	"encore.dev/beta/errs"
	"github.com/google/uuid"

	"encore.app/internal/clock"
)

// Provider is the external payment gateway.
//
// Shaped after Stripe's PaymentIntents: the caller supplies an idempotency key, and
// the provider distinguishes a *decline* (a definite business outcome) from an
// *error* (an ambiguous transport failure). Conflating those two is how systems end
// up either double-charging or giving away tickets.
type Provider interface {
	Charge(ctx context.Context, req ProviderCharge) (ProviderResult, error)
	Refund(ctx context.Context, req ProviderRefund) error
}

type ProviderCharge struct {
	IdempotencyKey string
	AmountCents    int64
	PaymentMethod  string
	Description    string
}

type ProviderResult struct {
	// Approved false is a decline: a definite "no", safe to record.
	Approved      bool
	ProviderRef   string
	DeclineReason string
}

type ProviderRefund struct {
	ProviderRef string
	Reason      string
}

// Behaviour modes for the mock provider.
const (
	ModeSucceed = "succeed"
	ModeDecline = "decline"
	// ModeError simulates a transport failure: the outcome is unknown, which is the
	// case that must leave the payment IN_PROGRESS rather than guessing.
	ModeError = "error"
	// ModeRefundFails proves compensation is treated as fallible.
	ModeRefundFails = "refund_fails"
)

// MockProvider is a scriptable in-memory provider used in local and test
// environments. It records call counts so a test can prove a charge happened exactly
// once across a retry.
//
// A real Stripe integration would implement the same interface; nothing outside this
// file knows which one is in use.
type MockProvider struct {
	mu      sync.Mutex
	mode    string
	charges int
	refunds int
	// chargedKeys makes the mock behave like a real provider with idempotency keys:
	// the same key returns the original result instead of charging again.
	chargedKeys map[string]ProviderResult
}

func NewMockProvider() *MockProvider {
	return &MockProvider{mode: ModeSucceed, chargedKeys: map[string]ProviderResult{}}
}

func (m *MockProvider) Charge(ctx context.Context, req ProviderCharge) (ProviderResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if prior, ok := m.chargedKeys[req.IdempotencyKey]; ok {
		// Provider-side idempotency: not counted as a new charge.
		return prior, nil
	}

	switch m.mode {
	case ModeError:
		m.charges++
		return ProviderResult{}, errors.New("simulated provider transport failure")
	case ModeDecline:
		m.charges++
		res := ProviderResult{Approved: false, DeclineReason: "card_declined"}
		m.chargedKeys[req.IdempotencyKey] = res
		return res, nil
	default:
		m.charges++
		res := ProviderResult{Approved: true, ProviderRef: "ch_" + uuid.NewString()}
		m.chargedKeys[req.IdempotencyKey] = res
		return res, nil
	}
}

func (m *MockProvider) Refund(ctx context.Context, req ProviderRefund) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refunds++
	if m.mode == ModeRefundFails {
		return errors.New("simulated refund failure")
	}
	return nil
}

func (m *MockProvider) SetMode(mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
}

func (m *MockProvider) Counts() (charges, refunds int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.charges, m.refunds
}

func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = ModeSucceed
	m.charges = 0
	m.refunds = 0
	m.chargedKeys = map[string]ProviderResult{}
}

// mockSingleton is shared so the testsupport endpoints can script the same provider
// instance the service is using.
var (
	mockOnce      sync.Once
	mockSingleton *MockProvider
)

// defaultProvider returns the mock in local and test environments.
//
// In any other environment it returns a provider that refuses every charge, so an
// unconfigured deployment fails closed instead of silently handing out free tickets.
func defaultProvider() Provider {
	if clock.IsTimeControllableEnv() {
		return sharedMock()
	}
	return unconfiguredProvider{}
}

func sharedMock() *MockProvider {
	mockOnce.Do(func() { mockSingleton = NewMockProvider() })
	return mockSingleton
}

// unconfiguredProvider is the production placeholder. Failing every charge is the
// safe direction: no real payment integration exists yet, and approving anything here
// would give tickets away for free.
type unconfiguredProvider struct{}

func (unconfiguredProvider) Charge(context.Context, ProviderCharge) (ProviderResult, error) {
	return ProviderResult{}, errors.New("no payment provider configured for this environment")
}

func (unconfiguredProvider) Refund(context.Context, ProviderRefund) error {
	return errors.New("no payment provider configured for this environment")
}

type SetBehaviourRequest struct {
	Mode string `json:"mode"`
}

type BehaviourResponse struct {
	Mode string `json:"mode"`
}

// SetTestBehaviour scripts the mock provider so sad paths are deterministic.
//
//encore:api private method=POST path=/internal/payments/test-behaviour
func SetTestBehaviour(ctx context.Context, req *SetBehaviourRequest) (*BehaviourResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	switch req.Mode {
	case ModeSucceed, ModeDecline, ModeError, ModeRefundFails:
	default:
		return nil, &errs.Error{Code: errs.InvalidArgument, Message: "unknown mode " + req.Mode}
	}
	sharedMock().SetMode(req.Mode)
	return &BehaviourResponse{Mode: req.Mode}, nil
}

type SummaryResponse struct {
	Charges int `json:"charges"`
	Refunds int `json:"refunds"`
}

// TestSummary reports provider call counts.
//
//encore:api private method=GET path=/internal/payments/test-summary
func TestSummary(ctx context.Context) (*SummaryResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	charges, refunds := sharedMock().Counts()
	return &SummaryResponse{Charges: charges, Refunds: refunds}, nil
}

// ResetTestProvider returns the mock to its default behaviour and clears counters.
//
//encore:api private method=POST path=/internal/payments/test-reset
func ResetTestProvider(ctx context.Context) (*BehaviourResponse, error) {
	if err := requireControllableEnv(); err != nil {
		return nil, err
	}
	sharedMock().Reset()
	return &BehaviourResponse{Mode: ModeSucceed}, nil
}

func requireControllableEnv() error {
	if clock.IsTimeControllableEnv() {
		return nil
	}
	return &errs.Error{
		Code:    errs.PermissionDenied,
		Message: "payment test controls are not available in this environment",
	}
}
