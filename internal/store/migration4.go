package store

// migration4 is phase 2: accounts, ownership, audit and integrity constraints (spec 1, 1b).
const migration4 = `
CREATE TABLE users (
	id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	username TEXT NOT NULL UNIQUE CHECK (username ~ '^[a-z][a-z0-9]{2,15}$'),
	role TEXT NOT NULL CHECK (role IN ('admin','user')),
	scram_salt BYTEA NOT NULL CHECK (octet_length(scram_salt) = 16),
	scram_iterations INT NOT NULL CHECK (scram_iterations BETWEEN 600000 AND 10000000),
	stored_key BYTEA NOT NULL CHECK (octet_length(stored_key) = 32),
	server_key BYTEA NOT NULL CHECK (octet_length(server_key) = 32),
	disabled BOOLEAN NOT NULL DEFAULT false,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	created_by BIGINT NULL REFERENCES users(id)
);
CREATE TABLE invites (
	id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	token_hash BYTEA NOT NULL UNIQUE,
	username TEXT NOT NULL CHECK (username ~ '^[a-z][a-z0-9]{2,15}$'),
	role TEXT NOT NULL CHECK (role IN ('admin','user')),
	purpose TEXT NOT NULL CHECK (purpose IN ('new_user','reset_password')),
	user_id BIGINT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_by BIGINT NULL REFERENCES users(id),
	expires_at TIMESTAMPTZ NOT NULL,
	used_at TIMESTAMPTZ NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	CHECK (expires_at > created_at),
	CHECK ((purpose = 'reset_password') = (user_id IS NOT NULL))
);
CREATE TABLE sessions (
	id_hash BYTEA PRIMARY KEY,
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	expires_at TIMESTAMPTZ NOT NULL,
	ip TEXT NOT NULL DEFAULT '',
	user_agent TEXT NOT NULL DEFAULT '',
	CHECK (expires_at > created_at)
);

ALTER TABLE databases ADD owner_id BIGINT NULL REFERENCES users(id);
CREATE TABLE database_members (
	database_id BIGINT NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
	user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	added_by BIGINT NULL REFERENCES users(id),
	added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (database_id, user_id)
);
ALTER TABLE destinations ADD owner_id BIGINT NULL REFERENCES users(id), ADD shared BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE restores ADD started_by_id BIGINT NULL REFERENCES users(id);
ALTER TABLE backups ADD started_by_id BIGINT NULL REFERENCES users(id);
CREATE TABLE dump_checks (
	id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	destination_id BIGINT NOT NULL,
	s3_key TEXT NOT NULL,
	status TEXT NOT NULL,
	result JSONB NOT NULL DEFAULT '{}',
	log TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	started_by_id BIGINT NULL REFERENCES users(id),
	started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	finished_at TIMESTAMPTZ NULL
);
CREATE TABLE image_checks (
	image_ref TEXT PRIMARY KEY,
	local_digest TEXT NOT NULL DEFAULT '',
	local_version TEXT NOT NULL DEFAULT '',
	remote_digest TEXT NOT NULL DEFAULT '',
	remote_version TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	checked_at TIMESTAMPTZ NOT NULL
);

-- 1. destinations: soft delete; name unique per owner among live rows
ALTER TABLE destinations ADD deleted_at TIMESTAMPTZ NULL;
ALTER TABLE destinations DROP CONSTRAINT destinations_name_key;
CREATE UNIQUE INDEX destinations_owner_name ON destinations (COALESCE(owner_id, 0), name) WHERE deleted_at IS NULL;
-- 2. widen destination ids
ALTER TABLE backups ALTER destination_id TYPE BIGINT;
ALTER TABLE restores ALTER destination_id TYPE BIGINT;
-- 3. tombstones for destinations deleted in phase 1 but still referenced
INSERT INTO destinations(id, name, provider, deleted_at)
	OVERRIDING SYSTEM VALUE
	SELECT x.id, '_deleted_' || x.id, 'deleted', now()
	FROM (SELECT destination_id AS id FROM backups UNION SELECT destination_id FROM restores) x
	WHERE x.id NOT IN (SELECT id FROM destinations);
-- 4. orphan schedule refs
UPDATE backups SET schedule_id = NULL WHERE schedule_id NOT IN (SELECT id FROM backup_schedules);
-- 5. FKs
ALTER TABLE backups ADD FOREIGN KEY (destination_id) REFERENCES destinations(id);
ALTER TABLE restores ADD FOREIGN KEY (destination_id) REFERENCES destinations(id);
ALTER TABLE dump_checks ADD FOREIGN KEY (destination_id) REFERENCES destinations(id);
ALTER TABLE backups ADD FOREIGN KEY (schedule_id) REFERENCES backup_schedules(id) ON DELETE SET NULL;
-- 6. CHECKs
ALTER TABLE databases
	ADD CHECK (status IN ('creating','running','stopped','error','deleted')),
	ADD CHECK (engine IN ('postgres','mysql','mariadb','mongo','redis')),
	ADD CHECK (tls_mode IN ('internal','acme'));
ALTER TABLE backups
	ADD CHECK (status IN ('running','ok','failed')),
	ADD CHECK (trigger IN ('schedule','manual','pre-delete','pre-restore')),
	ADD CHECK (size_bytes >= 0),
	ADD CHECK ((status = 'running') = (finished_at IS NULL)),
	ADD CHECK (status <> 'ok' OR s3_key <> '');
ALTER TABLE restores
	ADD CHECK (mode IN ('new','overwrite')),
	ADD CHECK (status IN ('running','ok','failed')),
	ADD CHECK ((status = 'running') = (finished_at IS NULL));
ALTER TABLE dump_checks
	ADD CHECK (status IN ('running','ok','failed')),
	ADD CHECK ((status = 'running') = (finished_at IS NULL));
ALTER TABLE backup_schedules ADD CHECK (keep_latest >= 1), ADD CHECK (cron <> '');
-- 6a. domains and node identity
CREATE TABLE domains (
	id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	owner_id BIGINT NULL REFERENCES users(id),
	name TEXT NOT NULL UNIQUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE databases
	ADD admin_user TEXT NOT NULL DEFAULT '',
	ADD admin_password_enc TEXT NOT NULL DEFAULT '',
	ADD node_key TEXT NOT NULL DEFAULT '',
	ADD domain_id BIGINT NULL REFERENCES domains(id) ON DELETE SET NULL,
	ADD CHECK (admin_user = '' OR admin_user ~ '^su_[0-9a-f]{16}$'),
	ADD CHECK (node_key = '' OR node_key ~ '^[0-9a-f]{16}$');
CREATE UNIQUE INDEX db_node_key ON databases(node_key) WHERE node_key <> '';
-- 6b. append-only audit log
CREATE TABLE audit_log (
	id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	at TIMESTAMPTZ NOT NULL DEFAULT now(),
	actor_id BIGINT NULL REFERENCES users(id),
	actor TEXT NOT NULL,
	ip TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL CHECK (action ~ '^[a-z_]+\.[a-z_]+$'),
	target_type TEXT NOT NULL DEFAULT '',
	target_id TEXT NOT NULL DEFAULT '',
	db_id BIGINT NULL REFERENCES databases(id),
	success BOOLEAN NOT NULL,
	detail JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_at ON audit_log (at DESC);
CREATE INDEX audit_actor ON audit_log (actor_id, at DESC);
CREATE INDEX audit_db ON audit_log (db_id, at DESC);
CREATE FUNCTION audit_log_guard() RETURNS trigger LANGUAGE plpgsql AS $f$
BEGIN
	IF TG_OP = 'DELETE' AND OLD.at < now() - interval '365 days' THEN
		RETURN OLD;
	END IF;
	RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'audit_log is append-only';
END $f$;
CREATE TRIGGER audit_log_guard BEFORE UPDATE OR DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_log_guard();
-- 7. the owner cannot also be a member
CREATE FUNCTION members_guard() RETURNS trigger LANGUAGE plpgsql AS $f$
BEGIN
	IF NEW.user_id = (SELECT owner_id FROM databases WHERE id = NEW.database_id) THEN
		RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'owner cannot be a member';
	END IF;
	RETURN NEW;
END $f$;
CREATE TRIGGER members_guard BEFORE INSERT OR UPDATE ON database_members FOR EACH ROW EXECUTE FUNCTION members_guard();
`
