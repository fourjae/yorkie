/*
 * Copyright 2021 The Yorkie Authors. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package housekeeping provides the housekeeping service. The housekeeping
// service is responsible for deactivating clients that have not been used for
// a long time.
package housekeeping

import (
	"context"
	"fmt"
	"time"

	"github.com/yorkie-team/yorkie/api/types"
	"github.com/yorkie-team/yorkie/server/backend/database"
	"github.com/yorkie-team/yorkie/server/backend/sync"
	"github.com/yorkie-team/yorkie/server/clients"
	"github.com/yorkie-team/yorkie/server/logging"
)

const (
	deactivateCandidatesKey = "housekeeping/deactivateCandidates"
	hardDeletionLockKey     = "housekeeping/hardDeletionLock"
)

// Housekeeping is the housekeeping service. It periodically runs housekeeping
// tasks. It is responsible for deactivating clients that have not been active
// for a long time.
type Housekeeping struct {
	database    database.Database
	coordinator sync.Coordinator

	interval                  time.Duration
	candidatesLimitPerProject int
	projectFetchSize          int

	ctx        context.Context
	cancelFunc context.CancelFunc
}

// Start starts the housekeeping service.
func Start(
	conf *Config,
	database database.Database,
	coordinator sync.Coordinator,
) (*Housekeeping, error) {
	h, err := New(conf, database, coordinator)
	if err != nil {
		return nil, err
	}
	if err := h.Start(); err != nil {
		return nil, err
	}

	return h, nil
}

// New creates a new housekeeping instance.
func New(
	conf *Config,
	database database.Database,
	coordinator sync.Coordinator,
) (*Housekeeping, error) {
	interval, err := time.ParseDuration(conf.Interval)
	if err != nil {
		return nil, fmt.Errorf("parse interval %s: %w", conf.Interval, err)
	}

	ctx, cancelFunc := context.WithCancel(context.Background())

	return &Housekeeping{
		database:    database,
		coordinator: coordinator,

		interval:                  interval,
		candidatesLimitPerProject: conf.CandidatesLimitPerProject,
		projectFetchSize:          conf.ProjectFetchSize,

		ctx:        ctx,
		cancelFunc: cancelFunc,
	}, nil
}

// Start starts the housekeeping service.
func (h *Housekeeping) Start() error {
	go h.AttachDeactivateCandidates()
	go h.AttachDocumentHardDeletion()
	return nil
}

// Stop stops the housekeeping service.
func (h *Housekeeping) Stop() error {
	h.cancelFunc()
	return nil
}

// AttachDeactivateCandidates is the housekeeping loop.
func (h *Housekeeping) AttachDeactivateCandidates() {
	housekeepingLastProjectID := database.DefaultProjectID

	for {
		ctx := context.Background()
		lastProjectID, err := h.deactivateCandidates(ctx, housekeepingLastProjectID)
		if err != nil {
			logging.From(ctx).Error(err)
			continue
		}

		housekeepingLastProjectID = lastProjectID

		select {
		case <-time.After(h.interval):
		case <-h.ctx.Done():
			return
		}
	}
}

func (h *Housekeeping) AttachDocumentHardDeletion() {
	housekeepingLastProjectID := database.DefaultProjectID

	for {
		ctx := context.Background()
		lastProjectID, err := h.documentHardDeletion(ctx, housekeepingLastProjectID)
		if err != nil {
			logging.From(ctx).Error(err)
			continue
		}

		housekeepingLastProjectID = lastProjectID

		select {
		case <-time.After(h.interval):
		case <-h.ctx.Done():
			return
		}
	}
}

func (h *Housekeeping) documentHardDeletion(
	ctx context.Context,
	housekeepingLastProjectID types.ID,
) (types.ID, error) {
	locker, err := h.coordinator.NewLocker(ctx, hardDeletionLockKey)
	if err != nil {
		return database.DefaultProjectID, err
	}
	if err := locker.Lock(ctx); err != nil {
		return database.DefaultProjectID, err
	}
	defer func() {
		if err := locker.Unlock(ctx); err != nil {
			logging.From(ctx).Error(err)
		}
	}()

	lastProjectID, candidates, err := h.database.FindHardDeletionCandidates(
		ctx,
		h.candidatesLimitPerProject,
		h.projectFetchSize,
		housekeepingLastProjectID,
	)

	if err != nil {
		return database.DefaultProjectID, err
	}

	lastProjectID, err = h.database.HardDeletion(ctx, candidates)

	if err != nil {
		return database.DefaultProjectID, err
	}

	return lastProjectID, err
}

// deactivateCandidates deactivates candidates.
func (h *Housekeeping) deactivateCandidates(
	ctx context.Context,
	housekeepingLastProjectID types.ID,
) (types.ID, error) {
	start := time.Now()
	locker, err := h.coordinator.NewLocker(ctx, deactivateCandidatesKey)
	if err != nil {
		return database.DefaultProjectID, err
	}

	if err := locker.Lock(ctx); err != nil {
		return database.DefaultProjectID, err
	}

	defer func() {
		if err := locker.Unlock(ctx); err != nil {
			logging.From(ctx).Error(err)
		}
	}()

	// FindDeactivateCandidates 메서드를 호출하여 비활성화할 대상이 되는 후보(candidates)를 데이터베이스에서 조회합니다.
	lastProjectID, candidates, err := h.database.FindDeactivateCandidates(
		ctx,
		h.candidatesLimitPerProject,
		h.projectFetchSize,
		housekeepingLastProjectID,
	)
	if err != nil {
		return database.DefaultProjectID, err
	}

	//조회된 후보들에 대해서 for 루프를 사용해 순회하면서 각각을 비활성화합니다.
	//clients.Deactivate 메서드를 사용하여 실제 비활성화 작업을 수행하고,
	//그 결과를 deactivatedCount 변수에 누적하여 비활성화된 항목의 수를 추적합니다.
	deactivatedCount := 0
	for _, clientInfo := range candidates {
		if _, err := clients.Deactivate(
			ctx,
			h.database,
			clientInfo.ProjectID,
			clientInfo.ID,
		); err != nil {
			return database.DefaultProjectID, err
		}

		deactivatedCount++
	}

	if len(candidates) > 0 {
		logging.From(ctx).Infof(
			"HSKP: candidates %d, deactivated %d, %s",
			len(candidates),
			deactivatedCount,
			time.Since(start),
		)
	}

	return lastProjectID, nil
}
