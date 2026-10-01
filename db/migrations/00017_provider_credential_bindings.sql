-- +goose Up
-- +goose StatementBegin

-- Metadata only: no key-value column, no runtime allocation or cloud access.
CREATE TABLE workspace_provider_credentials (
 id uuid PRIMARY KEY,
 workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 provider text NOT NULL CHECK (provider = 'claude_code'),
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','active','revoked')),
 epoch bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
 current_version_id uuid,
 created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT provider_credentials_workspace_provider_key UNIQUE(workspace_id,provider),
 CONSTRAINT provider_credentials_id_workspace_key UNIQUE(id,workspace_id),
 CHECK ((state='pending' AND epoch=0 AND current_version_id IS NULL)
     OR (state='active' AND epoch>0 AND current_version_id IS NOT NULL)
     OR (state='revoked' AND epoch>0 AND current_version_id IS NOT NULL))
);

-- Preserve resource ownership across rotations/disable. A version-only unique
-- index would let another tenant claim the same secret at a different version.
CREATE TABLE provider_credential_resources (
 id uuid PRIMARY KEY,
 workspace_id uuid NOT NULL,
 credential_id uuid NOT NULL,
 environment text NOT NULL CHECK (environment IN ('test','development','staging','production')),
 project_number text NOT NULL CHECK (project_number ~ '^[1-9][0-9]{5,19}$'),
 secret_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT credential_resources_owner_fkey FOREIGN KEY(credential_id,workspace_id)
     REFERENCES workspace_provider_credentials(id,workspace_id) ON DELETE CASCADE,
 CONSTRAINT credential_resources_identity_key UNIQUE(id,credential_id,workspace_id),
 CONSTRAINT credential_resources_global_key UNIQUE(project_number,secret_id)
);
CREATE INDEX credential_resources_workspace_idx ON provider_credential_resources(workspace_id);

CREATE TABLE provider_credential_versions (
 id uuid PRIMARY KEY,
 workspace_id uuid NOT NULL,
 credential_id uuid NOT NULL,
 resource_id uuid NOT NULL,
 secret_version bigint NOT NULL CHECK (secret_version>0),
 registration_epoch bigint NOT NULL CHECK (registration_epoch>0),
 credential_kind text NOT NULL DEFAULT 'api_key' CHECK (credential_kind='api_key'),
 verification_id uuid NOT NULL UNIQUE,
 created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
 created_at timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT credential_versions_resource_fkey FOREIGN KEY(resource_id,credential_id,workspace_id)
     REFERENCES provider_credential_resources(id,credential_id,workspace_id) ON DELETE CASCADE,
 CONSTRAINT credential_versions_identity_key UNIQUE(id,credential_id,workspace_id),
 CONSTRAINT credential_versions_epoch_key UNIQUE(credential_id,registration_epoch),
 CONSTRAINT credential_versions_resource_version_key UNIQUE(resource_id,secret_version)
);
CREATE INDEX credential_versions_workspace_idx ON provider_credential_versions(workspace_id);
ALTER TABLE workspace_provider_credentials ADD CONSTRAINT provider_credentials_current_version_fkey
 FOREIGN KEY(current_version_id,id,workspace_id)
 REFERENCES provider_credential_versions(id,credential_id,workspace_id);

ALTER TABLE workspace_provider_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_provider_credentials FORCE ROW LEVEL SECURITY;
CREATE POLICY provider_credentials_read ON workspace_provider_credentials FOR SELECT USING(workspace_id=weave_current_workspace_id());
CREATE POLICY provider_credentials_insert ON workspace_provider_credentials FOR INSERT WITH CHECK(workspace_id=weave_current_workspace_id());
CREATE POLICY provider_credentials_update ON workspace_provider_credentials FOR UPDATE USING(workspace_id=weave_current_workspace_id()) WITH CHECK(workspace_id=weave_current_workspace_id());
GRANT SELECT,INSERT,UPDATE ON workspace_provider_credentials TO weave_app;

ALTER TABLE provider_credential_resources ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_credential_resources FORCE ROW LEVEL SECURITY;
CREATE POLICY credential_resources_read ON provider_credential_resources FOR SELECT USING(workspace_id=weave_current_workspace_id());
CREATE POLICY credential_resources_insert ON provider_credential_resources FOR INSERT WITH CHECK(workspace_id=weave_current_workspace_id());
GRANT SELECT,INSERT ON provider_credential_resources TO weave_app;

ALTER TABLE provider_credential_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_credential_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY credential_versions_read ON provider_credential_versions FOR SELECT USING(workspace_id=weave_current_workspace_id());
CREATE POLICY credential_versions_insert ON provider_credential_versions FOR INSERT WITH CHECK(workspace_id=weave_current_workspace_id());
GRANT SELECT,INSERT ON provider_credential_versions TO weave_app;

CREATE FUNCTION weave_credential_metadata_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=public,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' AND pg_trigger_depth()>1
    AND NOT EXISTS(SELECT 1 FROM workspace_provider_credentials WHERE id=OLD.credential_id) THEN
  RETURN OLD;
 END IF;
 RAISE EXCEPTION 'provider credential metadata is immutable' USING ERRCODE='restrict_violation';
END;
$$;
CREATE TRIGGER credential_resources_immutable BEFORE UPDATE OR DELETE ON provider_credential_resources
 FOR EACH ROW EXECUTE FUNCTION weave_credential_metadata_immutable();
CREATE TRIGGER credential_resources_no_truncate BEFORE TRUNCATE ON provider_credential_resources
 FOR EACH STATEMENT EXECUTE FUNCTION weave_credential_metadata_immutable();
CREATE TRIGGER credential_versions_immutable BEFORE UPDATE OR DELETE ON provider_credential_versions
 FOR EACH ROW EXECUTE FUNCTION weave_credential_metadata_immutable();
CREATE TRIGGER credential_versions_no_truncate BEFORE TRUNCATE ON provider_credential_versions
 FOR EACH STATEMENT EXECUTE FUNCTION weave_credential_metadata_immutable();

CREATE FUNCTION weave_credential_binding_fence() RETURNS trigger
LANGUAGE plpgsql SET search_path=public,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  IF pg_trigger_depth()>1 AND NOT EXISTS(SELECT 1 FROM workspaces WHERE id=OLD.workspace_id) THEN
   RETURN OLD;
  END IF;
  RAISE EXCEPTION 'credential binding deletion refused' USING ERRCODE='restrict_violation';
 END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.state<>'pending' OR NEW.epoch<>0 OR NEW.current_version_id IS NOT NULL THEN
   RAISE EXCEPTION 'credential initial state refused' USING ERRCODE='check_violation';
  END IF;
  RETURN NEW;
 END IF;
 IF ROW(NEW.id,NEW.workspace_id,NEW.provider,NEW.created_by,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.id,OLD.workspace_id,OLD.provider,OLD.created_by,OLD.created_at)
    OR NEW.epoch<>OLD.epoch+1 THEN
  RAISE EXCEPTION 'credential identity or epoch refused' USING ERRCODE='check_violation';
 END IF;
 IF NEW.state='active' THEN
  IF NEW.current_version_id IS NOT DISTINCT FROM OLD.current_version_id
     OR NOT EXISTS(SELECT 1 FROM provider_credential_versions
       WHERE id=NEW.current_version_id AND credential_id=NEW.id AND workspace_id=NEW.workspace_id
         AND registration_epoch=NEW.epoch) THEN
   RAISE EXCEPTION 'credential version refused' USING ERRCODE='check_violation';
  END IF;
 ELSIF NEW.state='revoked' THEN
  IF OLD.state<>'active' OR NEW.current_version_id IS DISTINCT FROM OLD.current_version_id THEN
   RAISE EXCEPTION 'credential disable refused' USING ERRCODE='check_violation';
  END IF;
 ELSE
  RAISE EXCEPTION 'credential transition refused' USING ERRCODE='check_violation';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER credential_bindings_fenced BEFORE INSERT OR UPDATE OR DELETE ON workspace_provider_credentials
 FOR EACH ROW EXECUTE FUNCTION weave_credential_binding_fence();
REVOKE ALL ON FUNCTION weave_credential_metadata_immutable(),weave_credential_binding_fence() FROM PUBLIC;
COMMENT ON TABLE workspace_provider_credentials IS 'BYOK metadata status only. Active does not enable secret access or provider runtime.';
COMMENT ON TABLE provider_credential_resources IS 'Restricted immutable resource ownership. Names/labels alone are not verification.';
COMMENT ON TABLE provider_credential_versions IS 'Immutable numeric version and trusted verification identity, never key bytes.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workspace_provider_credentials DROP CONSTRAINT provider_credentials_current_version_fkey;
DROP TABLE provider_credential_versions;
DROP TABLE provider_credential_resources;
DROP TABLE workspace_provider_credentials;
DROP FUNCTION weave_credential_metadata_immutable();
DROP FUNCTION weave_credential_binding_fence();
-- +goose StatementEnd
