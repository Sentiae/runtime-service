package vmcomm

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// Pool manages a warm pool of pre-booted Firecracker VMs with connected agents.
// VMs are reused across multiple task executions. Idle VMs are terminated
// after IdleTimeoutMin to support scale-to-zero.
type Pool struct {
	mu             sync.Mutex
	vmProvider     usecase.VMProvider
	listener       *FirecrackerListener
	readyVMs       map[domain.Language][]*PooledVM // language → ready VMs
	busyVMs        map[uuid.UUID]*PooledVM         // vmID → busy VM
	allVMs         map[uuid.UUID]*PooledVM
	poolSize       int   // target number of warm VMs per language
	idleTimeoutMin int   // minutes before idle VMs are terminated (0 = never)
	totalBoots     int64 // total VMs booted (for metrics)
	totalTasks     int64 // total tasks executed
}

// PooledVM represents a VM in the pool with its vsock client.
type PooledVM struct {
	VM         *domain.MicroVM
	Client     *Client
	InUse      bool
	LastUsedAt time.Time
	TaskCount  int
}

// NewPool creates a warm pool manager.
// idleTimeoutMin controls how long idle VMs stay alive (0 = forever).
func NewPool(vmProvider usecase.VMProvider, listener *FirecrackerListener, poolSize int, idleTimeoutMin ...int) *Pool {
	if poolSize <= 0 {
		poolSize = 1
	}
	timeout := 0
	if len(idleTimeoutMin) > 0 {
		timeout = idleTimeoutMin[0]
	}
	p := &Pool{
		vmProvider:     vmProvider,
		listener:       listener,
		readyVMs:       make(map[domain.Language][]*PooledVM),
		busyVMs:        make(map[uuid.UUID]*PooledVM),
		allVMs:         make(map[uuid.UUID]*PooledVM),
		poolSize:       poolSize,
		idleTimeoutMin: timeout,
	}
	// Start idle cleanup goroutine if timeout > 0
	if timeout > 0 {
		go p.idleCleanupLoop()
	}
	return p
}

// idleCleanupLoop periodically terminates VMs that have been idle too long.
func (p *Pool) idleCleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		p.cleanupIdleVMs()
	}
}

func (p *Pool) cleanupIdleVMs() {
	p.mu.Lock()
	defer p.mu.Unlock()

	cutoff := time.Now().Add(-time.Duration(p.idleTimeoutMin) * time.Minute)

	for lang, vms := range p.readyVMs {
		kept := make([]*PooledVM, 0)
		for _, vm := range vms {
			if vm.LastUsedAt.Before(cutoff) {
				// Terminate idle VM
				log.Printf("pool: terminating idle VM %s (idle since %s)", vm.VM.ID, vm.LastUsedAt.Format(time.Kitchen))
				if vm.Client != nil {
					_ = vm.Client.Shutdown()
					_ = vm.Client.Close()
				}
				delete(p.allVMs, vm.VM.ID)
			} else {
				kept = append(kept, vm)
			}
		}
		p.readyVMs[lang] = kept
	}
}

// Stats returns pool usage statistics.
func (p *Pool) Stats() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	readyCount := 0
	for _, vms := range p.readyVMs {
		readyCount += len(vms)
	}

	return map[string]any{
		"ready_vms":    readyCount,
		"busy_vms":     len(p.busyVMs),
		"total_vms":    len(p.allVMs),
		"total_boots":  p.totalBoots,
		"total_tasks":  p.totalTasks,
		"pool_size":    p.poolSize,
		"idle_timeout": p.idleTimeoutMin,
	}
}

// AcquireVM gets a ready VM from the pool for the given language.
// If no warm VM is available, it boots a new one and waits for the agent.
func (p *Pool) AcquireVM(ctx context.Context, language domain.Language, resources domain.ResourceLimit) (*PooledVM, error) {
	p.mu.Lock()

	// Try to find a warm VM for this language
	if vms, ok := p.readyVMs[language]; ok && len(vms) > 0 {
		vm := vms[0]
		p.readyVMs[language] = vms[1:]
		vm.InUse = true
		p.busyVMs[vm.VM.ID] = vm
		p.mu.Unlock()
		log.Printf("pool: reusing warm VM %s for %s", vm.VM.ID, language)

		// Health-check the existing connection. If it's dead (broken pipe,
		// connection reset), reconnect to the agent via vsock UDS.
		// The agent stays alive and re-accepts connections.
		if err := p.ensureConnected(ctx, vm); err != nil {
			log.Printf("pool: warm VM %s connection dead, removing: %v", vm.VM.ID, err)
			p.mu.Lock()
			delete(p.busyVMs, vm.VM.ID)
			delete(p.allVMs, vm.VM.ID)
			p.mu.Unlock()
			// Fall through to boot a new VM
		} else {
			return vm, nil
		}
	} else {
		p.mu.Unlock()
	}

	// No warm VM — boot a new one
	log.Printf("pool: no warm VM for %s, booting new one...", language)
	return p.bootAndConnect(ctx, language, resources)
}

// ensureConnected verifies the VM's vsock connection is alive. If the
// connection is dead, it reconnects via the Firecracker vsock UDS and
// waits for the agent's Ready message (the agent re-accepts connections
// after a host disconnects).
func (p *Pool) ensureConnected(ctx context.Context, vm *PooledVM) error {
	// Quick check: try to ping the agent on the existing connection.
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := vm.Client.Ping(pingCtx); err == nil {
		return nil // connection is alive
	}
	log.Printf("pool: VM %s connection stale, reconnecting...", vm.VM.ID)

	// Close old connection.
	_ = vm.Client.Close()

	// Reconnect via vsock UDS.
	connectCtx, cancel2 := context.WithTimeout(ctx, 15*time.Second)
	defer cancel2()

	client, err := p.listener.ConnectToVM(connectCtx, vm.VM.ID, vm.VM.SocketPath)
	if err != nil {
		return fmt.Errorf("reconnect to VM %s: %w", vm.VM.ID, err)
	}

	// Wait for new Ready message (agent sends Ready on each new connection).
	info, err := client.WaitReady(connectCtx)
	if err != nil {
		client.Close()
		return fmt.Errorf("agent not ready after reconnect on VM %s: %w", vm.VM.ID, err)
	}

	log.Printf("pool: reconnected to VM %s (agent=%s)", vm.VM.ID, info.AgentVersion)
	vm.Client = client
	return nil
}

// ReleaseVM returns a VM to the pool for reuse.
func (p *Pool) ReleaseVM(vmID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()

	vm, ok := p.busyVMs[vmID]
	if !ok {
		return
	}
	delete(p.busyVMs, vmID)
	vm.InUse = false
	vm.LastUsedAt = time.Now()
	vm.TaskCount++
	p.totalTasks++

	// Return to ready pool
	lang := vm.VM.Language
	p.readyVMs[lang] = append(p.readyVMs[lang], vm)
	log.Printf("pool: VM %s returned to warm pool (%s)", vmID, lang)
}

// bootAndConnect boots a new VM and establishes vsock connection.
func (p *Pool) bootAndConnect(ctx context.Context, language domain.Language, resources domain.ResourceLimit) (*PooledVM, error) {
	vmID := uuid.New()
	bootCfg := usecase.VMBootConfig{
		VMID:        vmID,
		Language:    language,
		VCPU:        resources.VCPU,
		MemoryMB:    resources.MemoryMB,
		NetworkMode: domain.NetworkMode(resources.Network),
	}

	result, err := p.vmProvider.Boot(ctx, bootCfg)
	if err != nil {
		return nil, fmt.Errorf("boot VM: %w", err)
	}

	vm := &domain.MicroVM{
		ID:         vmID,
		Status:     domain.VMStatusRunning,
		VCPU:       resources.VCPU,
		MemoryMB:   resources.MemoryMB,
		Language:   language,
		SocketPath: result.SocketPath,
		BootTimeMS: &result.BootTimeMS,
	}

	// Connect to agent via vsock
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, err := p.listener.ConnectToVM(connectCtx, vmID, result.SocketPath)
	if err != nil {
		// Terminate the VM if we can't connect
		if result.PID > 0 {
			_ = p.vmProvider.Terminate(ctx, result.SocketPath, result.PID)
		}
		return nil, fmt.Errorf("connect to agent: %w", err)
	}

	// Wait for Ready
	info, err := client.WaitReady(connectCtx)
	if err != nil {
		client.Close()
		if result.PID > 0 {
			_ = p.vmProvider.Terminate(ctx, result.SocketPath, result.PID)
		}
		return nil, fmt.Errorf("agent not ready: %w", err)
	}

	log.Printf("pool: new VM %s ready (boot=%dms, agent=%s, langs=%v)",
		vmID, result.BootTimeMS, info.AgentVersion, info.SupportedLanguages)

	pooledVM := &PooledVM{
		VM:     vm,
		Client: client,
		InUse:  true,
	}

	p.mu.Lock()
	p.allVMs[vmID] = pooledVM
	p.busyVMs[vmID] = pooledVM
	p.mu.Unlock()

	return pooledVM, nil
}

// removeVM permanently removes a VM from the pool (e.g., broken connection).
func (p *Pool) removeVM(vmID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.busyVMs, vmID)
	delete(p.allVMs, vmID)
	for lang, vms := range p.readyVMs {
		for i, v := range vms {
			if v.VM.ID == vmID {
				p.readyVMs[lang] = append(vms[:i], vms[i+1:]...)
				break
			}
		}
	}
	log.Printf("pool: removed broken VM %s", vmID)
}

// Close terminates all VMs in the pool.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, vm := range p.allVMs {
		if vm.Client != nil {
			_ = vm.Client.Shutdown()
			_ = vm.Client.Close()
		}
	}
	p.readyVMs = make(map[domain.Language][]*PooledVM)
	p.busyVMs = make(map[uuid.UUID]*PooledVM)
	p.allVMs = make(map[uuid.UUID]*PooledVM)
}
