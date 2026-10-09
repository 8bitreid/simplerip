CREATE TABLE drive_settings (
    device     TEXT PRIMARY KEY,
    auto_eject BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
