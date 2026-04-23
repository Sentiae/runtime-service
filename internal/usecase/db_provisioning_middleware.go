package usecase

import (
	"context"

	"github.com/sentiae/runtime-service/internal/domain"
)

// DBLease is the handle returned by a Provisioner. URL is the
// DATABASE_URL the runner should inject into the test VM; Cleanup is
// called (best-effort) after the run completes regardless of outcome.
type DBLease struct {
	URL     string
	Cleanup func()
}

// Provisioner is the minimum interface the db-provisioning middleware
// needs. The existing testdb.Provisioner is wrapped via TestDBProvisioner
// so deploys that already ship an ephemeral-pg fleet can plug in
// without writing a new provisioner.
type Provisioner interface {
	Provision(ctx context.Context, run *domain.TestRun) (*DBLease, error)
}

// ProvisionerFunc is a minimal function adapter that satisfies
// Provisioner without the caller needing a named receiver type.
type ProvisionerFunc func(ctx context.Context, run *domain.TestRun) (*DBLease, error)

// Provision calls the wrapped function.
func (f ProvisionerFunc) Provision(ctx context.Context, run *domain.TestRun) (*DBLease, error) {
	return f(ctx, run)
}

// DBProvisioningMiddleware wraps any TestRunDispatcher with the §8.3
// database provisioning hook. If the run's DBMode is ephemeral_pg and
// a provisioner is wired, the middleware acquires a lease, injects its
// URL into the runner's EnvVars as DATABASE_URL, and defers Cleanup so
// the lease is released on both success and failure paths. A nil
// provisioner is a no-op, matching the contract where the dispatcher
// falls back to the inner surface unchanged.
type DBProvisioningMiddleware struct {
	inner       TestRunDispatcherIface
	provisioner Provisioner
}

// NewDBProvisioningMiddleware wires the middleware. Pass a nil
// provisioner to disable db-provisioning while still letting the
// chain be used uniformly.
func NewDBProvisioningMiddleware(inner TestRunDispatcherIface, p Provisioner) *DBProvisioningMiddleware {
	return &DBProvisioningMiddleware{inner: inner, provisioner: p}
}

// DispatchInVM implements TestRunDispatcherIface.
func (m *DBProvisioningMiddleware) DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error {
	if run.DBMode == domain.TestDBModeEphemeralPg && m.provisioner != nil {
		lease, err := m.provisioner.Provision(ctx, run)
		if err != nil {
			return err
		}
		if lease != nil {
			if lease.Cleanup != nil {
				defer lease.Cleanup()
			}
			if profile.EnvVars == nil {
				profile.EnvVars = make(map[string]string)
			}
			profile.EnvVars["DATABASE_URL"] = lease.URL
		}
	}
	if m.inner == nil {
		return nil
	}
	return m.inner.DispatchInVM(ctx, run, profile)
}
