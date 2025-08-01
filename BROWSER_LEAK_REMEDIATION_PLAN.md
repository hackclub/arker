# Browser Leak Remediation Plan

## Executive Summary
The archiving system is experiencing Chrome browser process leaks despite previous attempts to fix them. This plan provides a comprehensive, phased approach to eliminate all known browser leak sources in our Playwright-based archiving system.

## Root Cause Analysis
The Oracle identified four primary classes of browser leaks:

1. **Double Cleanup / Unreachable Cleanup**: `cleanup()` functions called twice or never reached due to panics
2. **Uncontrolled Goroutines**: Helper goroutines (network-idle, encoding, media checks) continue running after context cancellation 
3. **Event Listener Leaks**: `page.On(...)` listeners stay registered after page destruction
4. **Hanging Playwright APIs**: `page.Close()`, `browser.Close()`, `pw.Stop()` occasionally hang, leaving OS processes alive

## Implementation Plan

### Phase 1: Audit & Monitoring Infrastructure
**Goal**: Establish visibility and reproducible testing

#### Step 1.1: Add Browser Process Monitoring
- [ ] Add metrics endpoint that exposes:
  - Chrome process count via `pgrep -af chrome | wc -l`
  - Playwright launch/close/kill counters
  - Active goroutine count via `runtime.NumGoroutine()`

#### Step 1.2: Create Integration Test Suite
- [ ] Write test that:
  - Starts N workers
  - Feeds 1,000 mixed archiving jobs
  - Aborts 50% of jobs after 5 seconds (simulating timeouts)
  - Asserts final state: Chrome processes ≤ 2×workers, goroutine diff < 50

### Phase 2: Library-Level Fixes (Core Architecture)
**Goal**: Create bulletproof resource management primitives

#### Step 2.1: Implement Idempotent Cleanup Bundle
- [ ] Create `PWBundle` struct with:
  - Playwright instance tracking
  - Event listener cleanup tracking  
  - `sync.Once` for idempotent cleanup
  - Centralized resource disposal

#### Step 2.2: Fix Double Cleanup Pattern
- [ ] Replace all immediate `cleanup()` calls in error paths
- [ ] Modify archivers to return `PWBundle` even on failure
- [ ] Ensure workers handle cleanup via single `defer bundle.Cleanup()`

#### Step 2.3: Make Network Idle Detection Context-Aware
- [ ] Rewrite `waitForCustomNetworkIdle` to respect context cancellation
- [ ] Remove wrapper `waitForCustomNetworkIdleWithContext` function
- [ ] Add proper `select` statements with `ctx.Done()` checks

#### Step 2.4: Fix All Helper Goroutines
- [ ] Make `scrollToBottomAndWait` context-aware
- [ ] Fix `WaitForRobustPageLoad` context handling
- [ ] Use `errgroup.WithContext` for all background operations
- [ ] Ensure encoding goroutines respect context cancellation

#### Step 2.5: Implement Event Listener Lifecycle Management
- [ ] Create helper function to register listeners with automatic cleanup
- [ ] Track all `page.On()` calls for proper removal
- [ ] Integrate with `PWBundle` cleanup process

### Phase 3: Worker-Level Guarantees
**Goal**: Bulletproof resource cleanup at the worker boundary

#### Step 3.1: Implement Worker-Level Resource Management
- [ ] Add timeout-aware job processing with guaranteed cleanup
- [ ] Implement fallback cleanup when Archive() hangs
- [ ] Ensure cleanup happens regardless of success/failure/timeout

#### Step 3.2: Add Process Kill Failsafe
- [ ] Implement 5-second grace period after cleanup
- [ ] Add SIGKILL fallback for orphaned Chrome processes
- [ ] Track worker PIDs for targeted process cleanup

### Phase 4: Validation & Rollout
**Goal**: Verify fixes work in practice

#### Step 4.1: Local Testing
- [ ] Run integration test suite locally
- [ ] Verify stable process/goroutine counts
- [ ] Test with various failure scenarios

#### Step 4.2: Staging Deployment
- [ ] Deploy to staging with monitoring dashboard
- [ ] Run soak test with 10,000 jobs
- [ ] Monitor Chrome process count, memory usage, goroutines

#### Step 4.3: Production Rollout
- [ ] Gradual production deployment
- [ ] 24-hour stability monitoring
- [ ] Remove temporary metrics after validation

### Phase 5: Follow-ups (Optional)
**Goal**: Performance optimizations after leak elimination

#### Step 5.1: Performance Improvements
- [ ] Consider global Playwright instance reuse per worker
- [ ] Add Prometheus alerts for browser process count anomalies
- [ ] Optimize cold-start latency

## Success Criteria

### Immediate (Post Phase 3)
- [ ] Chrome process count remains stable during load testing
- [ ] No accumulation of orphaned browser processes
- [ ] Memory usage plateaus during continuous operation
- [ ] Integration tests pass consistently

### Long-term (Post Phase 4)
- [ ] Zero browser leak incidents for 7+ days
- [ ] System resource usage remains predictable
- [ ] Worker performance meets SLA requirements

## Risk Mitigation

### Rollback Plan
- Keep current worker implementation as backup
- Feature flag for new vs old archiver selection
- Immediate rollback capability if issues arise

### Monitoring
- Real-time Chrome process count alerts
- Memory usage trending
- Worker failure rate tracking
- Job completion time monitoring

## Timeline Estimate
- **Phase 1**: 1-2 days
- **Phase 2**: 3-4 days  
- **Phase 3**: 2-3 days
- **Phase 4**: 2-3 days
- **Total**: ~8-12 days

## Next Steps
1. Review and approve this plan
2. Begin Phase 1 implementation
3. Execute phases sequentially with approval gates
4. Validate each phase before proceeding

---
*This plan addresses all identified browser leak sources through systematic architectural improvements, comprehensive testing, and robust monitoring.*
