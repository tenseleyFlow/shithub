-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS SP13 — separate purchased Team licenses from used seats.
--
-- Earlier payment sprints treated "billable seats" as the current active
-- organization-member count. GitHub-style Team billing needs distinct concepts:
-- licensed seats are purchased capacity, while used seats are current consumers.
-- Keep billable_seats as a compatibility alias for licensed_seats until the UI,
-- Stripe update, and metrics paths are fully renamed.

-- +goose Up

ALTER TABLE org_billing_states
    ADD COLUMN licensed_seats integer NOT NULL DEFAULT 0,
    ADD COLUMN used_seats     integer NOT NULL DEFAULT 0;

WITH member_counts AS (
    SELECT org_id, count(*)::integer AS used_seats
    FROM org_members
    GROUP BY org_id
)
UPDATE org_billing_states s
   SET used_seats = COALESCE(member_counts.used_seats, 0),
       licensed_seats = CASE
           WHEN s.plan = 'team' THEN GREATEST(s.billable_seats, COALESCE(member_counts.used_seats, 0), 1)
           ELSE 0
       END,
       billable_seats = CASE
           WHEN s.plan = 'team' THEN GREATEST(s.billable_seats, COALESCE(member_counts.used_seats, 0), 1)
           ELSE s.billable_seats
       END,
       seat_snapshot_at = COALESCE(s.seat_snapshot_at, now()),
       updated_at = now()
  FROM member_counts
 WHERE s.org_id = member_counts.org_id;

UPDATE org_billing_states s
   SET used_seats = 0,
       licensed_seats = CASE
           WHEN s.plan = 'team' THEN GREATEST(s.billable_seats, 1)
           ELSE 0
       END,
       billable_seats = CASE
           WHEN s.plan = 'team' THEN GREATEST(s.billable_seats, 1)
           ELSE s.billable_seats
       END,
       updated_at = now()
 WHERE NOT EXISTS (
    SELECT 1 FROM org_members m WHERE m.org_id = s.org_id
 );

ALTER TABLE org_billing_states
    ADD CONSTRAINT org_billing_states_license_seats_nonnegative CHECK (
        licensed_seats >= 0 AND used_seats >= 0
    ),
    ADD CONSTRAINT org_billing_states_team_license_capacity CHECK (
        plan <> 'team' OR licensed_seats >= used_seats
    );

ALTER TABLE billing_seat_snapshots
    ADD COLUMN licensed_seats  integer NOT NULL DEFAULT 0,
    ADD COLUMN used_seats      integer NOT NULL DEFAULT 0,
    ADD COLUMN available_seats integer NOT NULL DEFAULT 0;

UPDATE billing_seat_snapshots
   SET licensed_seats = billable_seats,
       used_seats = active_members,
       available_seats = GREATEST(billable_seats - active_members, 0);

ALTER TABLE billing_seat_snapshots
    ADD CONSTRAINT billing_seat_snapshots_license_shape CHECK (
        licensed_seats >= 0
        AND used_seats >= 0
        AND available_seats >= 0
    );

-- +goose Down

ALTER TABLE billing_seat_snapshots
    DROP CONSTRAINT IF EXISTS billing_seat_snapshots_license_shape;

ALTER TABLE billing_seat_snapshots
    DROP COLUMN IF EXISTS available_seats,
    DROP COLUMN IF EXISTS used_seats,
    DROP COLUMN IF EXISTS licensed_seats;

ALTER TABLE org_billing_states
    DROP CONSTRAINT IF EXISTS org_billing_states_team_license_capacity,
    DROP CONSTRAINT IF EXISTS org_billing_states_license_seats_nonnegative;

ALTER TABLE org_billing_states
    DROP COLUMN IF EXISTS used_seats,
    DROP COLUMN IF EXISTS licensed_seats;
