// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/tenseleyFlow/shithub/internal/billing"
)

var (
	ErrOrgStorageQuotaExceeded = errors.New("entitlements: organization storage quota exceeded")
	ErrInvalidStorageQuotaUse  = errors.New("entitlements: storage quota usage cannot be negative")
)

type OrgStorageQuotaCheck struct {
	Allowed         bool
	UsedBytes       int64
	AdditionalBytes int64
	WouldUseBytes   int64
	LimitBytes      int64
	Unlimited       bool
	Overridden      bool
	RequiredPlan    billing.Plan
	Reason          Reason
}

type OrgStorageQuotaError struct {
	Check OrgStorageQuotaCheck
}

func (e *OrgStorageQuotaError) Error() string {
	if e == nil {
		return ErrOrgStorageQuotaExceeded.Error()
	}
	if msg := e.Check.Message(); msg != "" {
		return msg
	}
	return ErrOrgStorageQuotaExceeded.Error()
}

func (e *OrgStorageQuotaError) Unwrap() error {
	return ErrOrgStorageQuotaExceeded
}

func CheckOrgStorageQuota(ctx context.Context, deps Deps, orgID, usedBytes, additionalBytes int64) (OrgStorageQuotaCheck, error) {
	if usedBytes < 0 || additionalBytes < 0 {
		return OrgStorageQuotaCheck{}, ErrInvalidStorageQuotaUse
	}
	set, err := ForOrg(ctx, deps, orgID)
	if err != nil {
		return OrgStorageQuotaCheck{}, err
	}
	limit, err := set.Limit(LimitOrgStorageQuota)
	if err != nil {
		return OrgStorageQuotaCheck{}, err
	}
	check := OrgStorageQuotaCheck{
		Allowed:         true,
		UsedBytes:       usedBytes,
		AdditionalBytes: additionalBytes,
		WouldUseBytes:   usedBytes + additionalBytes,
		LimitBytes:      limit.Value,
		Unlimited:       limit.Unlimited || !limit.Defined,
		Overridden:      limit.Overridden,
		RequiredPlan:    limit.RequiredPlan,
		Reason:          limit.Reason,
	}
	if !limit.Allowed && limit.Reason != ReasonNone {
		check.Allowed = false
		check.Unlimited = false
		return check, nil
	}
	if check.Unlimited {
		return check, nil
	}
	if check.WouldUseBytes > limit.Value {
		check.Allowed = false
	}
	return check, nil
}

func (c OrgStorageQuotaCheck) Err() error {
	if c.Allowed {
		return nil
	}
	return &OrgStorageQuotaError{Check: c}
}

func (c OrgStorageQuotaCheck) Message() string {
	if c.Allowed || c.Unlimited {
		return ""
	}
	return fmt.Sprintf("Organization storage quota exceeded. This change would use %d bytes of %d bytes. Upgrade to Team or contact support to continue.", c.WouldUseBytes, c.LimitBytes)
}

func (c OrgStorageQuotaCheck) BillingPath(orgSlug string) string {
	return "/organizations/" + url.PathEscape(orgSlug) + "/settings/billing"
}

func (c OrgStorageQuotaCheck) UpgradeBanner(orgSlug string) UpgradeBanner {
	return UpgradeBanner{
		Message:    c.Message(),
		ActionText: "Manage billing and plans",
		ActionHref: c.BillingPath(orgSlug),
		StatusCode: c.HTTPStatus(),
	}
}

func (c OrgStorageQuotaCheck) HTTPStatus() int {
	if c.Allowed {
		return 200
	}
	return 402
}
