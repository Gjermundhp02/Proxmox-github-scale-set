package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/Telmate/proxmox-api-go/proxmox"
	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

type Scaler struct {
	runners        runnerState
	runnerImage    string
	scaleSetID     int
	proxmoxClient  *proxmox.Client
	proxmoxNode    string
	proxmoxStorage string
	proxmoxOSTmpl  string
	scalesetClient *scaleset.Client
	minRunners     int
	maxRunners     int
	logger         *slog.Logger
}

func (a *Scaler) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	currentCount := a.runners.count()
	targetRunnerCount := min(a.maxRunners, a.minRunners+count)

	switch {
	case targetRunnerCount == currentCount:
		// No scaling needed
		return currentCount, nil
	case targetRunnerCount > currentCount:
		// Scale up
		scaleUp := targetRunnerCount - currentCount
		a.logger.Info(
			"Scaling up runners",
			slog.Int("currentCount", currentCount),
			slog.Int("desiredCount", targetRunnerCount),
			slog.Int("scaleUp", scaleUp),
		)

		for range scaleUp {
			if _, err := a.startRunner(ctx); err != nil {
				return 0, fmt.Errorf("failed to start runner: %w", err)
			}
		}

		return a.runners.count(), nil
	default:
		// No need to handle scale down events, since:
		// 1. JobCompleted events will first remove runners
		// 2. If the count is still below the current runner count, the JobCompleted event will be delivered in the next batch.
		// 3. Removal after JobCompleted events is handled synchronously.
		// 4. If the job is cancelled, the JobCompleted event will still be delivered.
	}
	return a.runners.count(), nil
}

func (a *Scaler) HandleJobStarted(ctx context.Context, jobInfo *scaleset.JobStarted) error {
	a.logger.Info(
		"Job started",
		slog.Int64("runnerRequestId", jobInfo.RunnerRequestID),
		slog.String("jobId", jobInfo.JobID),
	)
	a.runners.markBusy(jobInfo.RunnerName)
	return nil
}

func (a *Scaler) HandleJobCompleted(ctx context.Context, jobInfo *scaleset.JobCompleted) error {
	a.logger.Info("Job completed", slog.Int64("runnerRequestId", jobInfo.RunnerRequestID), slog.String("jobId", jobInfo.JobID))

	vmIDStr := a.runners.markDone(jobInfo.RunnerName)
	vmID, err := strconv.Atoi(vmIDStr)
	if err != nil {
		return fmt.Errorf("invalid vm id stored for runner %s: %w", jobInfo.RunnerName, err)
	}

	vmr, err := a.proxmoxClient.GetVmRefById(ctx, proxmox.GuestID(vmID))
	if err != nil {
		return fmt.Errorf("failed to get proxmox vm ref: %w", err)
	}
	if _, err := a.proxmoxClient.DeleteVm(ctx, vmr); err != nil {
		return fmt.Errorf("failed to delete proxmox vm: %w", err)
	}

	return nil
}

func (a *Scaler) startRunner(ctx context.Context) (string, error) {
	name := fmt.Sprintf("runner-%s", uuid.NewString()[:8])

	jit, err := a.scalesetClient.GenerateJitRunnerConfig(
		ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{
			Name: name,
		},
		a.scaleSetID,
	)
	if err != nil {
		return "", fmt.Errorf("failed to generate JIT config: %w", err)
	}
	// jit is needed by runners; currently we don't have a direct way to inject
	// the JIT config into the LXC during creation. Keep the value available
	// for future integration (e.g., cloud-init or guest-agent). Silence unused
	// variable for now.
	_ = jit

	// Allocate next ID
	nextID, err := a.proxmoxClient.GetNextID(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get next proxmox id: %w", err)
	}
	vmid := int(nextID)

	params := map[string]interface{}{
		"vmid":       vmid,
		"hostname":   name,
		"ostemplate": a.proxmoxOSTmpl,
		"storage":    a.proxmoxStorage,
		"memory":     512,
		"cores":      1,
		"rootfs":     "1G",
	}

	if _, err := a.proxmoxClient.CreateLxcContainer(ctx, a.proxmoxNode, params); err != nil {
		return "", fmt.Errorf("failed to create lxc container: %w", err)
	}

	// Lookup the created container and start it
	vmr, err := a.proxmoxClient.GetVmRefByName(ctx, proxmox.GuestName(name))
	if err != nil {
		return "", fmt.Errorf("failed to find created lxc container: %w", err)
	}
	if _, err := a.proxmoxClient.StartVm(ctx, vmr); err != nil {
		return "", fmt.Errorf("failed to start created lxc container: %w", err)
	}

	a.runners.addIdle(name, strconv.Itoa(vmid))
	return name, nil
}

func (a *Scaler) shutdown(ctx context.Context) {
	a.logger.Info("Shutting down runners")
	a.runners.mu.Lock()
	defer a.runners.mu.Unlock()

	for name, containerID := range a.runners.idle {
		a.logger.Info("Removing idle runner", slog.String("name", name), slog.String("vmid", containerID))
		vmid, err := strconv.Atoi(containerID)
		if err != nil {
			a.logger.Error("Invalid vm id", slog.String("name", name), slog.String("vmid", containerID), slog.String("error", err.Error()))
			continue
		}
		vmr, err := a.proxmoxClient.GetVmRefById(ctx, proxmox.GuestID(vmid))
		if err != nil {
			a.logger.Error("Failed to get vm ref", slog.String("vmid", containerID), slog.String("error", err.Error()))
			continue
		}
		if _, err := a.proxmoxClient.DeleteVm(ctx, vmr); err != nil {
			a.logger.Error("Failed to delete idle vm", slog.String("name", name), slog.String("vmid", containerID), slog.String("error", err.Error()))
		}
	}
	clear(a.runners.idle)

	for name, containerID := range a.runners.busy {
		a.logger.Info("Removing busy runner", slog.String("name", name), slog.String("vmid", containerID))
		vmid, err := strconv.Atoi(containerID)
		if err != nil {
			a.logger.Error("Invalid vm id", slog.String("name", name), slog.String("vmid", containerID), slog.String("error", err.Error()))
			continue
		}
		vmr, err := a.proxmoxClient.GetVmRefById(ctx, proxmox.GuestID(vmid))
		if err != nil {
			a.logger.Error("Failed to get vm ref", slog.String("vmid", containerID), slog.String("error", err.Error()))
			continue
		}
		if _, err := a.proxmoxClient.DeleteVm(ctx, vmr); err != nil {
			a.logger.Error("Failed to delete busy vm", slog.String("name", name), slog.String("vmid", containerID), slog.String("error", err.Error()))
		}
	}
	clear(a.runners.busy)
}

var _ listener.Scaler = (*Scaler)(nil)

type runnerState struct {
	mu   sync.Mutex
	idle map[string]string
	busy map[string]string
}

func (r *runnerState) count() int {
	r.mu.Lock()
	count := len(r.idle) + len(r.busy)
	r.mu.Unlock()
	return count
}

func (r *runnerState) markBusy(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.idle[name]
	if !ok {
		panic("marking non-existent runner busy")
	}
	delete(r.idle, name)
	r.busy[name] = state
}

func (r *runnerState) markDone(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.markDoneUnlocked(name)
}

func (r *runnerState) markDoneUnlocked(name string) string {
	containerID, ok := r.busy[name]
	if ok {
		delete(r.busy, name)
		return containerID
	}
	containerID, ok = r.idle[name]
	if ok {
		delete(r.idle, name)
		return containerID
	}
	panic("marking non-existent runner done")
}

func (r *runnerState) addIdle(name, containerID string) {
	r.mu.Lock()
	r.idle[name] = containerID
	r.mu.Unlock()
}
