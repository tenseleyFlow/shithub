-- +goose Up
-- +goose StatementBegin
ALTER TABLE push_events DROP CONSTRAINT push_events_protocol;
ALTER TABLE push_events
    ADD CONSTRAINT push_events_protocol
    CHECK (protocol IN ('http', 'ssh', 'web'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE push_events DROP CONSTRAINT push_events_protocol;
ALTER TABLE push_events
    ADD CONSTRAINT push_events_protocol
    CHECK (protocol IN ('http', 'ssh'));
-- +goose StatementEnd
