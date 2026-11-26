-- Custom server actions. The actions themselves live in the server
-- definition files; who may run them is stored here, one row per grant.
CREATE TABLE action_permissions (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    server_id  TEXT    NOT NULL,
    action_id  TEXT    NOT NULL,
    granted_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, server_id, action_id)
);
CREATE INDEX action_permissions_server ON action_permissions(server_id, action_id);

-- Action runs are recorded in the operation history (type 'action').
ALTER TABLE operations ADD COLUMN action_id TEXT NOT NULL DEFAULT '';
ALTER TABLE operations ADD COLUMN output    TEXT NOT NULL DEFAULT '';
CREATE INDEX operations_action ON operations(server_id, action_id, id);
