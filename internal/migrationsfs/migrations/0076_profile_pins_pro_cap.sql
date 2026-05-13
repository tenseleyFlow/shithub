-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO07: raise the profile_pins position cap from 6 to 100 so Pro
-- users can pin more than the Free baseline. The cap stays small —
-- 100 is "effectively unlimited for product copy, bounded for DB
-- sanity" per PRO01. The Free cap stays at entitlements.FreeProfilePinsCap;
-- enforcement lives in the handler (selectedProfilePinIDs + the per-
-- feature enforce flag), this migration only relaxes the table
-- constraint that backed the pre-PRO07 single cap.

-- +goose Up
ALTER TABLE profile_pins
    DROP CONSTRAINT profile_pins_position_range;
ALTER TABLE profile_pins
    ADD CONSTRAINT profile_pins_position_range CHECK (position BETWEEN 1 AND 100);

-- +goose Down
ALTER TABLE profile_pins
    DROP CONSTRAINT profile_pins_position_range;
ALTER TABLE profile_pins
    ADD CONSTRAINT profile_pins_position_range CHECK (position BETWEEN 1 AND 6);
