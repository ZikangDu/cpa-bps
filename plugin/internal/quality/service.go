package quality

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Runner func(context.Context, Check) Result

type Options struct {
	Defaults    Config
	Timeout     time.Duration
	Cooldown    time.Duration
	Tick        time.Duration
	GlobalLimit int
	Now         func() time.Time
}

type Service struct {
	mu                          sync.Mutex
	path                        string
	state                       state
	defaults                    Config
	runner                      Runner
	now                         func() time.Time
	timeout, cooldown, tick     time.Duration
	active                      map[string]string
	activeCancels               map[string]context.CancelFunc
	slots                       chan struct{}
	wake                        chan struct{}
	ctx                         context.Context
	cancel                      context.CancelFunc
	wg                          sync.WaitGroup
	started, closed, stateError bool
	available                   bool
}

// New only reads local state. No timer or upstream call starts until Start.
func New(path string, runner Runner, options Options) (*Service, error) {
	if path == "" || runner == nil || ValidateConfig(options.Defaults) != nil {
		return nil, ErrInvalid
	}
	saved, err := readState(path)
	if err != nil {
		return nil, err
	}
	if options.Timeout <= 0 {
		options.Timeout = 90 * time.Second
	}
	if options.Cooldown <= 0 {
		options.Cooldown = 30 * time.Second
	}
	if options.Tick <= 0 {
		options.Tick = 10 * time.Second
	}
	if options.GlobalLimit < 1 || options.GlobalLimit > 2 {
		options.GlobalLimit = 1
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{path: path, state: saved, defaults: cloneConfig(options.Defaults), runner: runner, now: options.Now,
		timeout: options.Timeout, cooldown: options.Cooldown, tick: options.Tick, active: map[string]string{}, activeCancels: map[string]context.CancelFunc{},
		slots: make(chan struct{}, options.GlobalLimit), wake: make(chan struct{}, 1), available: true}, nil
}

func (s *Service) Path() string { return s.path }

func (s *Service) SetAvailable(available bool) {
	s.mu.Lock()
	s.available = available
	if !available {
		for _, cancel := range s.activeCancels {
			cancel()
		}
	}
	s.signal()
	s.mu.Unlock()
}

// CancelRunning cancels observations captured under an old plugin configuration.
// It does not change saved plans or account business state.
func (s *Service) CancelRunning() {
	s.mu.Lock()
	for _, cancel := range s.activeCancels {
		cancel()
	}
	s.mu.Unlock()
}

func (s *Service) Start(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.started = true
	s.wg.Add(1)
	go s.loop()
}

func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Service) Snapshot(index string) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, found := s.state.Accounts[index]
	if !found {
		account.Config = cloneConfig(s.defaults)
	}
	result := Snapshot{AuthIndex: index, Config: cloneConfig(account.Config), NextDue: account.NextDue,
		LastStarted: account.LastStarted, Running: s.active[index] != "", RunID: s.active[index], Blocked: account.Blocked,
		ReservedUntil: account.ReservedUntil,
		StateError:    s.stateError, Results: []Result{}}
	for i := len(s.state.Results) - 1; i >= 0; i-- {
		if s.state.Results[i].AuthIndex == index {
			result.Results = append(result.Results, s.state.Results[i])
		}
	}
	return result
}

func (s *Service) Put(index, authID string, cfg Config) error {
	if !validIndex(index) || authID == "" || len(authID) > 1024 || ValidateConfig(cfg) != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.stateError {
		return ErrUnavailable
	}
	previous, found := s.state.Accounts[index]
	if !found && len(s.state.Accounts) >= maxAccounts {
		return ErrUnavailable
	}
	if found && previous.AuthID != authID {
		return ErrInvalid
	}
	next := previous
	next.AuthID = authID
	next.Config = cloneConfig(cfg)
	next.Revision++
	if cfg.Enabled {
		// An explicit enabled save is the only way to resume after a policy block.
		next.Blocked = false
		if previous.NextDue.IsZero() || !previous.Config.Enabled || cfg.IntervalMinutes != previous.Config.IntervalMinutes {
			next.NextDue = s.now().UTC().Add(time.Duration(cfg.IntervalMinutes) * time.Minute)
		}
	} else {
		next.NextDue = time.Time{}
	}
	s.state.Accounts[index] = next
	if err := writeState(s.path, s.state); err != nil {
		if found {
			s.state.Accounts[index] = previous
		} else {
			delete(s.state.Accounts, index)
		}
		return err
	}
	if cancel := s.activeCancels[index]; cancel != nil {
		cancel()
	}
	s.signal()
	return nil
}

// Run reserves a batch durably before launching it. It does not wait on network
// work, and refuses duplicate account runs even while they await a global slot.
func (s *Service) Run(index, authID string, request RunRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validIndex(index) || authID == "" || len(authID) > 1024 {
		return "", ErrInvalid
	}
	account, found := s.state.Accounts[index]
	if !found {
		account = accountState{AuthID: authID, Config: cloneConfig(s.defaults)}
	}
	if account.AuthID != authID {
		return "", ErrInvalid
	}
	cfg := cloneConfig(account.Config)
	if len(request.Models) > 0 {
		cfg.Models = append([]string(nil), request.Models...)
	}
	if request.ReasoningEffort != "" {
		cfg.ReasoningEffort = request.ReasoningEffort
	}
	if len(cfg.Models) == 0 || ValidateConfig(cfg) != nil {
		return "", ErrInvalid
	}
	return s.launchLocked(index, account, cfg, true)
}

func (s *Service) launchLocked(index string, account accountState, cfg Config, manual bool) (string, error) {
	if s.closed || !s.started || !s.available {
		return "", ErrClosed
	}
	if s.stateError {
		return "", ErrUnavailable
	}
	if account.Blocked && !manual {
		return "", ErrBlocked
	}
	if s.active[index] != "" {
		return "", ErrBusy
	}
	now := s.now().UTC()
	if now.Before(account.ReservedUntil) {
		return "", ErrBusy
	}
	if !account.LastStarted.IsZero() && now.Sub(account.LastStarted) < s.cooldown {
		return "", ErrCooldown
	}
	previous, found := s.state.Accounts[index]
	if !found && len(s.state.Accounts) >= maxAccounts {
		return "", ErrUnavailable
	}
	// Claim the global slot before reserving or creating a goroutine. Due plans
	// retry on the next tick; manual callers get a visible busy response.
	select {
	case s.slots <- struct{}{}:
	default:
		return "", ErrBusy
	}
	launched := false
	defer func() {
		if !launched {
			<-s.slots
		}
	}()
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", ErrUnavailable
	}
	id := hex.EncodeToString(raw)
	account.LastStarted = now
	account.ReservedUntil = now.Add(s.timeout * time.Duration(len(cfg.Models)))
	// Reserve the request deadline as well as the interval before sending. A
	// restart never immediately replays a possibly issued request.
	if !manual && account.Config.Enabled {
		account.NextDue = now.Add(time.Duration(account.Config.IntervalMinutes)*time.Minute + s.timeout*time.Duration(len(cfg.Models)))
	}
	s.state.Accounts[index] = account
	if err := writeState(s.path, s.state); err != nil {
		if found {
			s.state.Accounts[index] = previous
		} else {
			delete(s.state.Accounts, index)
		}
		return "", err
	}
	s.active[index] = id
	batchContext, batchCancel := context.WithCancel(s.ctx)
	s.activeCancels[index] = batchCancel
	s.wg.Add(1)
	launched = true
	go s.execute(batchContext, index, account.AuthID, id, cfg, account.Revision, manual)
	return id, nil
}

func (s *Service) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		s.runDue()
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Service) runDue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.stateError || !s.available {
		return
	}
	now := s.now()
	indexes := make([]string, 0, len(s.state.Accounts))
	for index, account := range s.state.Accounts {
		if account.Config.Enabled && !account.Blocked && !account.NextDue.IsZero() && !now.Before(account.NextDue) {
			indexes = append(indexes, index)
		}
	}
	sort.Slice(indexes, func(i, j int) bool {
		left, right := s.state.Accounts[indexes[i]].NextDue, s.state.Accounts[indexes[j]].NextDue
		if left.Equal(right) {
			return indexes[i] < indexes[j]
		}
		return left.Before(right)
	})
	for _, index := range indexes {
		account := s.state.Accounts[index]
		_, _ = s.launchLocked(index, account, cloneConfig(account.Config), false)
	}
}

func (s *Service) execute(batchContext context.Context, index, authID, runID string, cfg Config, revision uint64, manual bool) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		defer func() { <-s.slots; s.signal(); s.mu.Unlock() }()
		delete(s.active, index)
		if cancel := s.activeCancels[index]; cancel != nil {
			cancel()
			delete(s.activeCancels, index)
		}
		account := s.state.Accounts[index]
		if s.ctx.Err() == nil {
			account.ReservedUntil = time.Time{}
			if !manual && account.Config.Enabled && account.Revision == revision && !account.Blocked {
				account.NextDue = s.now().UTC().Add(time.Duration(account.Config.IntervalMinutes) * time.Minute)
			}
			s.state.Accounts[index] = account
			if writeState(s.path, s.state) != nil {
				s.stateError = true
			}
		}
	}()
	for i, model := range cfg.Models {
		if batchContext.Err() != nil {
			s.recordSkippedTail(index, runID, cfg, i, manual, "bps_quality_skipped_canceled")
			return
		}
		s.mu.Lock()
		changed := s.state.Accounts[index].Revision != revision || !s.available
		s.mu.Unlock()
		if changed {
			s.recordSkippedTail(index, runID, cfg, i, manual, "bps_quality_skipped_config_changed")
			return
		}
		started := s.now().UTC()
		ctx, cancel := context.WithTimeout(batchContext, s.timeout)
		result := s.runner(ctx, Check{AuthIndex: index, AuthID: authID, Model: model, Config: cloneConfig(cfg), RunID: runID, Manual: manual})
		if ctx.Err() == context.DeadlineExceeded {
			result.Status = "timeout"
			result.ErrorCode = "bps_quality_timeout"
		} else if ctx.Err() == context.Canceled {
			result.Status = "skipped"
			result.ErrorCode = "bps_quality_canceled"
		}
		cancel()
		result.ID = fmt.Sprintf("%s-%d", runID, i+1)
		result.RunID = runID
		result.Manual = manual
		result.AuthIndex = index
		result.RequestedModel = model
		result.StartedAt = started
		result.CompletedAt = s.now().UTC()
		result.ElapsedMS = max(0, result.CompletedAt.Sub(started).Milliseconds())
		result.ReasoningEffort = cfg.ReasoningEffort
		result.ExpectedAnswer = SafeText(cfg.ExpectedAnswer, 128)
		result.Prompt = SafeText(cfg.Prompt, 8192)
		if !validStatus(result.Status) {
			result.Status = "request_error"
			result.ErrorCode = "bps_quality_invalid_result"
		}
		result.Answer = SafeText(result.Answer, MaxAnswerBytes)
		result.ExtractedAnswer = SafeText(result.ExtractedAnswer, 128)
		if !ValidModel(result.ActualModel) {
			result.ActualModel = ""
		}
		result.Usage.InputTokens = max(0, result.Usage.InputTokens)
		result.Usage.OutputTokens = max(0, result.Usage.OutputTokens)
		result.Usage.TotalTokens = max(0, result.Usage.TotalTokens)
		s.mu.Lock()
		s.state.Results = append(s.state.Results, result)
		s.pruneLocked()
		blocked := result.ErrorCode == "bps_usage_policy_blocked"
		if blocked && !manual {
			account := s.state.Accounts[index]
			account.Blocked = true
			account.Config.Enabled = false
			account.NextDue = time.Time{}
			s.state.Accounts[index] = account
		}
		if writeState(s.path, s.state) != nil {
			s.stateError = true
		}
		failedSave := s.stateError
		s.mu.Unlock()
		if blocked {
			s.recordSkippedTail(index, runID, cfg, i+1, manual, "bps_quality_skipped_after_policy")
			return
		}
		if failedSave {
			s.recordSkippedTail(index, runID, cfg, i+1, manual, "bps_quality_skipped_state_unavailable")
			return
		}
	}
}

// An accepted model is either observed or explicitly recorded as unexecuted.
// Never send more requests after a policy rejection merely to fill the history.
func (s *Service) recordSkippedTail(index, runID string, cfg Config, start int, manual bool, reason string) {
	if start >= len(cfg.Models) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	for position := start; position < len(cfg.Models); position++ {
		s.state.Results = append(s.state.Results, Result{
			ID: fmt.Sprintf("%s-%d", runID, position+1), RunID: runID, AuthIndex: index,
			RequestedModel: cfg.Models[position], Status: "skipped", Manual: manual,
			StartedAt: now, CompletedAt: now, ErrorCode: reason,
			Prompt: SafeText(cfg.Prompt, 8192), ExpectedAnswer: SafeText(cfg.ExpectedAnswer, 128),
			ReasoningEffort: cfg.ReasoningEffort, BenchmarkVersion: BenchmarkVersion(cfg),
		})
	}
	s.pruneLocked()
	if !s.stateError && writeState(s.path, s.state) != nil {
		s.stateError = true
	}
}

func (s *Service) pruneLocked() {
	counts := map[string]int{}
	keep := make([]Result, 0, min(len(s.state.Results), maxResults))
	for i := len(s.state.Results) - 1; i >= 0 && len(keep) < maxResults; i-- {
		result := s.state.Results[i]
		key := result.AuthIndex + "\x00" + result.RequestedModel
		if counts[key] < HistoryPerModel {
			counts[key]++
			keep = append(keep, result)
		}
	}
	for left, right := 0, len(keep)-1; left < right; left, right = left+1, right-1 {
		keep[left], keep[right] = keep[right], keep[left]
	}
	s.state.Results = keep
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func cloneConfig(c Config) Config { c.Models = append([]string{}, c.Models...); return c }
func validIndex(s string) bool {
	return strings.TrimSpace(s) == s && s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n")
}
func validStatus(s string) bool {
	switch s {
	case "pass", "answer_fail", "request_error", "timeout", "skipped":
		return true
	}
	return false
}
