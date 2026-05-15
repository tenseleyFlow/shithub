-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-04: profile vanity pack. Pro users can customize the
-- accent color and pin layout used on their public profile page.
--
-- Both columns are stored on the user row directly (not a side table)
-- because every profile view reads them and a join is wasted. The
-- handler-side `FeatureProfileVanity` gate prevents Free users from
-- writing non-default values; setting one as Pro and then lapsing to
-- Free leaves the stored value in place — the renderer still respects
-- it, matching the campaign's "Pro configuration preserved on
-- downgrade" rule (PRO06's read-only-after-downgrade pattern).
--
-- The `''` empty default lets a Free user round-trip the form without
-- needing a default applied at handler time. Accent hex is validated
-- in Go against the strict `#rrggbb` shape before insert; the column
-- only stores already-validated values.

-- +goose Up
ALTER TABLE users
    ADD COLUMN profile_accent_hex text NOT NULL DEFAULT '',
    ADD COLUMN profile_layout     text NOT NULL DEFAULT 'list';

ALTER TABLE users
    ADD CONSTRAINT users_profile_accent_hex_shape CHECK (
        profile_accent_hex = ''
        OR profile_accent_hex ~ '^#[0-9a-f]{6}$'
    );

ALTER TABLE users
    ADD CONSTRAINT users_profile_layout_known CHECK (
        profile_layout IN ('list', 'featured')
    );

-- +goose Down
ALTER TABLE users DROP CONSTRAINT users_profile_layout_known;
ALTER TABLE users DROP CONSTRAINT users_profile_accent_hex_shape;
ALTER TABLE users DROP COLUMN profile_layout;
ALTER TABLE users DROP COLUMN profile_accent_hex;
