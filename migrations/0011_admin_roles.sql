CREATE TABLE admin_users (
    username        text PRIMARY KEY CHECK (length(username) BETWEEN 3 AND 64),
    password_hash   text NOT NULL,
    role            text NOT NULL CHECK (role IN ('admin','question_bank_editor','reviewer','publisher')),
    active          boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE admin_user_audit (
    id              bigserial PRIMARY KEY,
    actor           text NOT NULL,
    username        text NOT NULL,
    action          text NOT NULL CHECK (action IN ('created','updated')),
    old_role        text,
    new_role        text NOT NULL CHECK (new_role IN ('admin','question_bank_editor','reviewer','publisher')),
    old_active      boolean,
    new_active      boolean NOT NULL,
    password_changed boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_admin_user_audit_username ON admin_user_audit (username,id);

CREATE OR REPLACE FUNCTION reject_admin_user_audit_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'admin user audit rows are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER admin_user_audit_immutable
    BEFORE UPDATE OR DELETE ON admin_user_audit
    FOR EACH ROW EXECUTE FUNCTION reject_admin_user_audit_mutation();
