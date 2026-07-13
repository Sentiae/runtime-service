CREATE TABLE fleet_secret_bindings (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    app_id     UUID         NOT NULL REFERENCES fleet_apps(id) ON DELETE CASCADE,
    secret_ref VARCHAR(255) NOT NULL,
    inject_as  VARCHAR(10)  NOT NULL DEFAULT 'env',
    target     VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX fleet_secret_bindings_app_id_idx ON fleet_secret_bindings (app_id);
