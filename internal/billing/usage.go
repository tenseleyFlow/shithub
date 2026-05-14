// SPDX-License-Identifier: AGPL-3.0-or-later

package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
)

type (
	QuotaKind     = billingdb.OrgQuotaKind
	UsageCounter  = billingdb.OrgUsageCounter
	UsageSnapshot = billingdb.OrgUsageSnapshot
	QuotaOverride = billingdb.OrgQuotaOverride
)

const (
	QuotaKindStorageBytes   = billingdb.OrgQuotaKindStorageBytes
	QuotaKindActionsMinutes = billingdb.OrgQuotaKindActionsMinutes
)

var (
	ErrInvalidUsageCounter  = errors.New("billing: usage counters cannot be negative")
	ErrInvalidUsagePeriod   = errors.New("billing: usage period is invalid")
	ErrInvalidUsageLimit    = errors.New("billing: usage recalc limit is invalid")
	ErrInvalidQuotaKind     = errors.New("billing: invalid quota kind")
	ErrInvalidQuotaOverride = errors.New("billing: invalid quota override")
)

type UsageCounterSnapshot struct {
	OrgID                int64
	RepoStorageBytes     int64
	ObjectStorageBytes   int64
	ActionsLogBytes      int64
	ActionsArtifactBytes int64
	ActionsMinutesUsed   int64
	ActionsPeriodStart   time.Time
	ActionsPeriodEnd     time.Time
	CalculatedAt         time.Time
}

type QuotaOverrideInput struct {
	OrgID           int64
	Kind            QuotaKind
	LimitValue      int64
	Unlimited       bool
	Note            string
	CreatedByUserID int64
}

func MonthlyUsagePeriod(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

func GetOrgUsageCounters(ctx context.Context, deps Deps, orgID int64) (UsageCounter, error) {
	if err := validateDeps(deps); err != nil {
		return UsageCounter{}, err
	}
	if orgID == 0 {
		return UsageCounter{}, ErrOrgIDRequired
	}
	return billingdb.New().GetOrgUsageCounters(ctx, deps.Pool, orgID)
}

func UpsertOrgUsageCounters(ctx context.Context, deps Deps, snap UsageCounterSnapshot) (UsageCounter, error) {
	if err := validateDeps(deps); err != nil {
		return UsageCounter{}, err
	}
	if err := validateUsageSnapshot(snap); err != nil {
		return UsageCounter{}, err
	}
	return billingdb.New().UpsertOrgUsageCounters(ctx, deps.Pool, billingdb.UpsertOrgUsageCountersParams{
		OrgID:                snap.OrgID,
		RepoStorageBytes:     snap.RepoStorageBytes,
		ObjectStorageBytes:   snap.ObjectStorageBytes,
		ActionsLogBytes:      snap.ActionsLogBytes,
		ActionsArtifactBytes: snap.ActionsArtifactBytes,
		ActionsMinutesUsed:   snap.ActionsMinutesUsed,
		ActionsPeriodStart:   pgTime(snap.ActionsPeriodStart),
		ActionsPeriodEnd:     pgTime(snap.ActionsPeriodEnd),
		CalculatedAt:         pgTime(snap.CalculatedAt),
	})
}

func RecalculateOrgUsageCounters(ctx context.Context, deps Deps, orgID int64, periodStart, periodEnd time.Time) (UsageCounter, error) {
	if err := validateDeps(deps); err != nil {
		return UsageCounter{}, err
	}
	if orgID == 0 {
		return UsageCounter{}, ErrOrgIDRequired
	}
	if periodStart.IsZero() || periodEnd.IsZero() || !periodStart.Before(periodEnd) {
		return UsageCounter{}, ErrInvalidUsagePeriod
	}
	row, err := billingdb.New().RecalculateOrgUsageCounters(ctx, deps.Pool, billingdb.RecalculateOrgUsageCountersParams{
		OrgID:              orgID,
		ActionsPeriodStart: pgTime(periodStart),
		ActionsPeriodEnd:   pgTime(periodEnd),
	})
	if err != nil {
		return UsageCounter{}, err
	}
	return UsageCounter(row), nil
}

func ListActiveOrgIDsForUsageRecalc(ctx context.Context, deps Deps, limit int32) ([]int64, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, ErrInvalidUsageLimit
	}
	return billingdb.New().ListActiveOrgIDsForUsageRecalc(ctx, deps.Pool, limit)
}

func CreateOrgUsageSnapshot(ctx context.Context, deps Deps, orgID int64, source string) (UsageSnapshot, error) {
	if err := validateDeps(deps); err != nil {
		return UsageSnapshot{}, err
	}
	if orgID == 0 {
		return UsageSnapshot{}, ErrOrgIDRequired
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "local"
	}
	return billingdb.New().CreateOrgUsageSnapshot(ctx, deps.Pool, billingdb.CreateOrgUsageSnapshotParams{
		OrgID:  orgID,
		Source: source,
	})
}

func ListOrgUsageSnapshots(ctx context.Context, deps Deps, orgID int64, limit int32) ([]UsageSnapshot, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if orgID == 0 {
		return nil, ErrOrgIDRequired
	}
	if limit <= 0 {
		limit = 10
	}
	return billingdb.New().ListOrgUsageSnapshots(ctx, deps.Pool, billingdb.ListOrgUsageSnapshotsParams{
		OrgID: orgID,
		Limit: limit,
	})
}

func ListOrgQuotaOverrides(ctx context.Context, deps Deps, orgID int64) ([]QuotaOverride, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if orgID == 0 {
		return nil, ErrOrgIDRequired
	}
	return billingdb.New().ListOrgQuotaOverrides(ctx, deps.Pool, orgID)
}

func GetOrgQuotaOverride(ctx context.Context, deps Deps, orgID int64, kind QuotaKind) (QuotaOverride, error) {
	if err := validateDeps(deps); err != nil {
		return QuotaOverride{}, err
	}
	if orgID == 0 {
		return QuotaOverride{}, ErrOrgIDRequired
	}
	if !validQuotaKind(kind) {
		return QuotaOverride{}, ErrInvalidQuotaKind
	}
	return billingdb.New().GetOrgQuotaOverride(ctx, deps.Pool, billingdb.GetOrgQuotaOverrideParams{
		OrgID: orgID,
		Kind:  kind,
	})
}

func UpsertOrgQuotaOverride(ctx context.Context, deps Deps, in QuotaOverrideInput) (QuotaOverride, error) {
	if err := validateDeps(deps); err != nil {
		return QuotaOverride{}, err
	}
	if in.OrgID == 0 {
		return QuotaOverride{}, ErrOrgIDRequired
	}
	if !validQuotaKind(in.Kind) {
		return QuotaOverride{}, ErrInvalidQuotaKind
	}
	if !in.Unlimited && in.LimitValue < 0 {
		return QuotaOverride{}, ErrInvalidQuotaOverride
	}
	return billingdb.New().UpsertOrgQuotaOverride(ctx, deps.Pool, billingdb.UpsertOrgQuotaOverrideParams{
		OrgID:           in.OrgID,
		Kind:            in.Kind,
		LimitValue:      pgOptionalInt8(in.LimitValue, !in.Unlimited),
		Unlimited:       in.Unlimited,
		Note:            strings.TrimSpace(in.Note),
		CreatedByUserID: pgOptionalInt8(in.CreatedByUserID, in.CreatedByUserID != 0),
	})
}

func DeleteOrgQuotaOverride(ctx context.Context, deps Deps, orgID int64, kind QuotaKind) (int64, error) {
	if err := validateDeps(deps); err != nil {
		return 0, err
	}
	if orgID == 0 {
		return 0, ErrOrgIDRequired
	}
	if !validQuotaKind(kind) {
		return 0, ErrInvalidQuotaKind
	}
	return billingdb.New().DeleteOrgQuotaOverride(ctx, deps.Pool, billingdb.DeleteOrgQuotaOverrideParams{
		OrgID: orgID,
		Kind:  kind,
	})
}

func validateUsageSnapshot(snap UsageCounterSnapshot) error {
	if snap.OrgID == 0 {
		return ErrOrgIDRequired
	}
	if snap.RepoStorageBytes < 0 ||
		snap.ObjectStorageBytes < 0 ||
		snap.ActionsLogBytes < 0 ||
		snap.ActionsArtifactBytes < 0 ||
		snap.ActionsMinutesUsed < 0 {
		return ErrInvalidUsageCounter
	}
	if snap.ActionsPeriodStart.IsZero() || snap.ActionsPeriodEnd.IsZero() || !snap.ActionsPeriodStart.Before(snap.ActionsPeriodEnd) {
		return ErrInvalidUsagePeriod
	}
	return nil
}

func validQuotaKind(kind QuotaKind) bool {
	switch kind {
	case QuotaKindStorageBytes, QuotaKindActionsMinutes:
		return true
	default:
		return false
	}
}

func pgOptionalInt8(v int64, valid bool) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: valid}
}
