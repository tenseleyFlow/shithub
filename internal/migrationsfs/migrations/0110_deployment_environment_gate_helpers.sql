-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP23 deployment-environment branch pattern matching.
--
-- GitHub environment deployment branch policies accept simple wildcard
-- patterns for selected branches/tags. Runner dispatch evaluates those
-- patterns inside the claim query so blocked jobs remain queued rather
-- than being assigned to a runner and cancelled after the fact.

-- +goose Up

CREATE OR REPLACE FUNCTION shithub_deployment_pattern_matches(pattern text, ref_name text)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
STRICT
AS $$
DECLARE
    regex text := '^';
    i integer := 1;
    ch text;
BEGIN
    WHILE i <= char_length(pattern) LOOP
        ch := substr(pattern, i, 1);
        IF ch = '*' THEN
            regex := regex || '.*';
        ELSIF ch = '?' THEN
            regex := regex || '.';
        ELSIF position(ch in '\.+^$()[]{}|') > 0 THEN
            regex := regex || '\' || ch;
        ELSE
            regex := regex || ch;
        END IF;
        i := i + 1;
    END LOOP;

    regex := regex || '$';
    RETURN ref_name ~ regex;
END;
$$;

-- +goose Down

DROP FUNCTION IF EXISTS shithub_deployment_pattern_matches(text, text);
