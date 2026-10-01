-- +goose Up
-- +goose StatementBegin
-- Capability roles only; never attach a login/service identity here.
DO $$
DECLARE r text;
BEGIN
 FOREACH r IN ARRAY ARRAY['weave_secret_approval_reader','weave_secret_approval_writer'] LOOP
  IF NOT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r) THEN
   EXECUTE format('CREATE ROLE %I NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS',r);
  END IF;
  IF pg_has_role('weave_app',r,'MEMBER') THEN
   RAISE EXCEPTION 'ordinary app inherits secret approval capability' USING ERRCODE='check_violation';
  END IF;
  IF EXISTS(SELECT 1 FROM pg_roles WHERE rolname=r AND (rolcanlogin OR rolinherit OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)) THEN
   RAISE EXCEPTION 'unsafe secret approval capability role' USING ERRCODE='check_violation';
  END IF;
 END LOOP;
END $$;
GRANT USAGE ON SCHEMA public TO weave_secret_approval_reader,weave_secret_approval_writer;
GRANT EXECUTE ON FUNCTION weave_current_workspace_id() TO weave_secret_approval_reader,weave_secret_approval_writer;

-- Global minimal nonreuse root: no workspace, actor, content or observation.
CREATE TABLE provider_secret_reservations (
 project_number text NOT NULL CHECK(project_number ~ '^[1-9][0-9]{5,19}$'),
 secret_id uuid NOT NULL CHECK(secret_id<>'00000000-0000-0000-0000-000000000000'),
 environment text NOT NULL CHECK(environment IN ('test','development','staging','production')),
 resource_family text NOT NULL DEFAULT 'gcp_global' CHECK(resource_family='gcp_global'),
 state text NOT NULL DEFAULT 'reserved' CHECK(state IN ('reserved','consumed','retired')),
 PRIMARY KEY(project_number,secret_id),
 UNIQUE(project_number,secret_id,environment,resource_family)
);
-- Tenant intent is separate so it can cascade while the root persists.
CREATE TABLE provider_secret_intents (
 id uuid PRIMARY KEY CHECK(id<>'00000000-0000-0000-0000-000000000000'),
 workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 provider text NOT NULL CHECK(provider='claude_code'),
 environment text NOT NULL,
 project_number text NOT NULL,
 secret_id uuid NOT NULL,
 resource_family text NOT NULL DEFAULT 'gcp_global' CHECK(resource_family='gcp_global'),
 initial_version bigint NOT NULL CHECK(initial_version>0),
 principal_id uuid NOT NULL CHECK(principal_id<>'00000000-0000-0000-0000-000000000000'),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(id,workspace_id), UNIQUE(project_number,secret_id),
 FOREIGN KEY(project_number,secret_id,environment,resource_family)
 REFERENCES provider_secret_reservations(project_number,secret_id,environment,resource_family)
);
CREATE TABLE provider_secret_assignments (
 intent_id uuid PRIMARY KEY,
 workspace_id uuid NOT NULL,
 secret_seconds bigint NOT NULL CHECK(secret_seconds BETWEEN 0 AND 253402300799),
 secret_nanos integer NOT NULL CHECK(secret_nanos BETWEEN 0 AND 999999999),
 principal_id uuid NOT NULL CHECK(principal_id<>'00000000-0000-0000-0000-000000000000'),
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(secret_seconds<>0 OR secret_nanos<>0),
 UNIQUE(intent_id,workspace_id),
 FOREIGN KEY(intent_id,workspace_id) REFERENCES provider_secret_intents(id,workspace_id) ON DELETE CASCADE
);
CREATE TABLE provider_secret_approvals (
 id uuid PRIMARY KEY CHECK(id<>'00000000-0000-0000-0000-000000000000'),
 intent_id uuid NOT NULL,
 workspace_id uuid NOT NULL,
 secret_version bigint NOT NULL CHECK(secret_version>0),
 version_seconds bigint NOT NULL CHECK(version_seconds BETWEEN 0 AND 253402300799),
 version_nanos integer NOT NULL CHECK(version_nanos BETWEEN 0 AND 999999999),
 schema_version integer NOT NULL DEFAULT 1 CHECK(schema_version=1),
 credential_kind text NOT NULL DEFAULT 'api_key' CHECK(credential_kind='api_key'),
 principal_id uuid NOT NULL CHECK(principal_id<>'00000000-0000-0000-0000-000000000000'),
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(version_seconds<>0 OR version_nanos<>0),
 UNIQUE(id,intent_id,workspace_id), UNIQUE(intent_id,secret_version),
 FOREIGN KEY(intent_id,workspace_id) REFERENCES provider_secret_assignments(intent_id,workspace_id) ON DELETE CASCADE
);
CREATE TABLE provider_secret_withdrawals (
 id uuid PRIMARY KEY CHECK(id<>'00000000-0000-0000-0000-000000000000'),
 intent_id uuid NOT NULL,
 workspace_id uuid NOT NULL,
 approval_id uuid,
 principal_id uuid NOT NULL CHECK(principal_id<>'00000000-0000-0000-0000-000000000000'),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE NULLS NOT DISTINCT(intent_id,approval_id),
 FOREIGN KEY(intent_id,workspace_id) REFERENCES provider_secret_intents(id,workspace_id) ON DELETE CASCADE,
 FOREIGN KEY(approval_id,intent_id,workspace_id) REFERENCES provider_secret_approvals(id,intent_id,workspace_id) ON DELETE CASCADE
);
CREATE INDEX secret_intents_workspace_idx ON provider_secret_intents(workspace_id);
CREATE INDEX secret_assignments_workspace_idx ON provider_secret_assignments(workspace_id);
CREATE INDEX secret_approvals_workspace_idx ON provider_secret_approvals(workspace_id);
CREATE INDEX secret_withdrawals_workspace_idx ON provider_secret_withdrawals(workspace_id);

ALTER TABLE provider_secret_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_secret_reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY secret_reservation_scope ON provider_secret_reservations TO weave_secret_approval_writer
 USING(environment=NULLIF(current_setting('app.secret_environment',true),'') AND project_number=NULLIF(current_setting('app.secret_project',true),''))
 WITH CHECK(environment=NULLIF(current_setting('app.secret_environment',true),'') AND project_number=NULLIF(current_setting('app.secret_project',true),''));
GRANT SELECT,INSERT,UPDATE(state) ON provider_secret_reservations TO weave_secret_approval_writer;

ALTER TABLE provider_secret_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_secret_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY secret_intent_read ON provider_secret_intents FOR SELECT TO weave_secret_approval_reader,weave_secret_approval_writer
 USING(workspace_id=weave_current_workspace_id() AND environment=NULLIF(current_setting('app.secret_environment',true),'') AND project_number=NULLIF(current_setting('app.secret_project',true),''));
CREATE POLICY secret_intent_write ON provider_secret_intents FOR INSERT TO weave_secret_approval_writer
 WITH CHECK(workspace_id=weave_current_workspace_id() AND environment=NULLIF(current_setting('app.secret_environment',true),'') AND project_number=NULLIF(current_setting('app.secret_project',true),''));

-- Assignment/approval/withdrawal visibility also requires a visible scoped intent.
DO $$
DECLARE t text;
BEGIN
 FOREACH t IN ARRAY ARRAY['provider_secret_assignments','provider_secret_approvals','provider_secret_withdrawals'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',t);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',t);
  EXECUTE format('CREATE POLICY secret_child_read ON %I FOR SELECT TO weave_secret_approval_reader,weave_secret_approval_writer USING(workspace_id=weave_current_workspace_id() AND EXISTS(SELECT 1 FROM provider_secret_intents i WHERE i.id=intent_id AND i.workspace_id=%I.workspace_id))',t,t);
  EXECUTE format('CREATE POLICY secret_child_write ON %I FOR INSERT TO weave_secret_approval_writer WITH CHECK(workspace_id=weave_current_workspace_id() AND EXISTS(SELECT 1 FROM provider_secret_intents i WHERE i.id=intent_id AND i.workspace_id=%I.workspace_id))',t,t);
 END LOOP;
END $$;
GRANT SELECT ON provider_secret_intents,provider_secret_assignments,provider_secret_approvals,provider_secret_withdrawals TO weave_secret_approval_reader;
GRANT SELECT,INSERT ON provider_secret_intents,provider_secret_assignments,provider_secret_approvals,provider_secret_withdrawals TO weave_secret_approval_writer;

CREATE FUNCTION weave_secret_root_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=public,pg_temp AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.state<>'reserved' THEN RAISE EXCEPTION 'secret initial state refused' USING ERRCODE='check_violation'; END IF;
  RETURN NEW;
 ELSIF TG_OP='UPDATE' THEN
  IF ROW(NEW.project_number,NEW.secret_id,NEW.environment,NEW.resource_family) IS DISTINCT FROM ROW(OLD.project_number,OLD.secret_id,OLD.environment,OLD.resource_family)
     OR NOT ((OLD.state='reserved' AND NEW.state IN ('consumed','retired')) OR (OLD.state='consumed' AND NEW.state='retired')) THEN
   RAISE EXCEPTION 'secret root transition refused' USING ERRCODE='check_violation';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM provider_secret_intents WHERE project_number=OLD.project_number AND secret_id=OLD.secret_id)
     AND NOT (OLD.state='reserved' AND NEW.state='retired' AND EXISTS(SELECT 1 FROM provider_secret_reservations WHERE project_number=OLD.project_number AND secret_id=OLD.secret_id AND xmin=pg_current_xact_id()::xid)) THEN
   RAISE EXCEPTION 'secret retirement intent unavailable' USING ERRCODE='check_violation';
  END IF;
  RETURN NEW;
 END IF;
 RAISE EXCEPTION 'secret reservation cannot be erased' USING ERRCODE='restrict_violation';
END $$;
CREATE TRIGGER secret_root_guard BEFORE INSERT OR UPDATE OR DELETE ON provider_secret_reservations FOR EACH ROW EXECUTE FUNCTION weave_secret_root_guard();
CREATE TRIGGER secret_root_no_truncate BEFORE TRUNCATE ON provider_secret_reservations FOR EACH STATEMENT EXECUTE FUNCTION weave_secret_root_guard();

-- A committed reservation must have its durable intent before cloud I/O.
CREATE FUNCTION weave_secret_root_claimed() RETURNS trigger LANGUAGE plpgsql SET search_path=public,pg_temp AS $$
DECLARE s text; i uuid;
BEGIN
 SELECT state INTO s FROM provider_secret_reservations WHERE project_number=NEW.project_number AND secret_id=NEW.secret_id;
 SELECT id INTO i FROM provider_secret_intents WHERE project_number=NEW.project_number AND secret_id=NEW.secret_id;
 IF (s IN ('reserved','consumed') AND i IS NULL)
    OR (s='consumed' AND NOT EXISTS(SELECT 1 FROM provider_secret_assignments WHERE intent_id=i))
    OR (s='retired' AND i IS NOT NULL AND NOT EXISTS(SELECT 1 FROM provider_secret_withdrawals WHERE intent_id=i AND approval_id IS NULL)) THEN
  RAISE EXCEPTION 'secret reservation missing durable decision' USING ERRCODE='check_violation';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER secret_root_claimed AFTER INSERT OR UPDATE ON provider_secret_reservations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION weave_secret_root_claimed();

CREATE FUNCTION weave_secret_intent_claim() RETURNS trigger LANGUAGE plpgsql SET search_path=public,pg_temp AS $$
DECLARE s text; fresh boolean;
BEGIN
 -- Only the transaction that INSERTed the name may attach its first intent.
 -- xid8->xid comparison uses the current transaction's live 32-bit identity,
 -- not a stored numeric txid; historic MVCC observations are not authority.
 SELECT state,xmin=pg_current_xact_id()::xid INTO s,fresh FROM provider_secret_reservations WHERE project_number=NEW.project_number AND secret_id=NEW.secret_id FOR UPDATE;
 IF s IS DISTINCT FROM 'reserved' OR fresh IS DISTINCT FROM true THEN RAISE EXCEPTION 'secret name already claimed' USING ERRCODE='check_violation'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER secret_intent_claim BEFORE INSERT ON provider_secret_intents FOR EACH ROW EXECUTE FUNCTION weave_secret_intent_claim();

-- Shared root row lock serializes approval publication with withdrawal.
CREATE FUNCTION weave_secret_decision_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=public,pg_temp AS $$
DECLARE p text; resource uuid; s text; sec bigint; ns integer; initial bigint;
BEGIN
 SELECT project_number,secret_id INTO p,resource FROM provider_secret_intents WHERE id=NEW.intent_id AND workspace_id=NEW.workspace_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'secret intent unavailable' USING ERRCODE='check_violation'; END IF;
 SELECT state INTO s FROM provider_secret_reservations WHERE project_number=p AND secret_id=resource FOR UPDATE;
 IF TG_TABLE_NAME='provider_secret_assignments' THEN
  IF s IS DISTINCT FROM 'reserved' THEN RAISE EXCEPTION 'secret resource unavailable' USING ERRCODE='check_violation'; END IF;
  UPDATE provider_secret_reservations SET state='consumed' WHERE project_number=p AND secret_id=resource;
  RETURN NEW;
 END IF;
 IF s IS DISTINCT FROM 'consumed' THEN
  IF TG_TABLE_NAME<>'provider_secret_withdrawals' THEN RAISE EXCEPTION 'secret resource unavailable' USING ERRCODE='check_violation'; END IF;
  IF s IS DISTINCT FROM 'reserved' OR NEW.approval_id IS NOT NULL THEN RAISE EXCEPTION 'secret resource unavailable' USING ERRCODE='check_violation'; END IF;
 END IF;
 IF TG_TABLE_NAME='provider_secret_withdrawals' THEN
  IF NEW.approval_id IS NULL THEN UPDATE provider_secret_reservations SET state='retired' WHERE project_number=p AND secret_id=resource; END IF;
 ELSIF TG_TABLE_NAME='provider_secret_approvals' THEN
  SELECT initial_version INTO initial FROM provider_secret_intents WHERE id=NEW.intent_id AND workspace_id=NEW.workspace_id;
  IF NEW.secret_version<initial OR (NEW.secret_version<>initial AND NOT EXISTS(SELECT 1 FROM provider_secret_approvals WHERE intent_id=NEW.intent_id AND workspace_id=NEW.workspace_id AND secret_version=initial)) THEN
   RAISE EXCEPTION 'secret initial version refused' USING ERRCODE='check_violation';
  END IF;
  SELECT secret_seconds,secret_nanos INTO sec,ns FROM provider_secret_assignments WHERE intent_id=NEW.intent_id AND workspace_id=NEW.workspace_id;
  IF NOT FOUND OR ROW(NEW.version_seconds,NEW.version_nanos)<ROW(sec,ns) THEN
   RAISE EXCEPTION 'secret observation refused' USING ERRCODE='check_violation';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER secret_assignment_guard BEFORE INSERT ON provider_secret_assignments FOR EACH ROW EXECUTE FUNCTION weave_secret_decision_guard();
CREATE TRIGGER secret_approval_guard BEFORE INSERT ON provider_secret_approvals FOR EACH ROW EXECUTE FUNCTION weave_secret_decision_guard();
CREATE TRIGGER secret_withdrawal_guard BEFORE INSERT ON provider_secret_withdrawals FOR EACH ROW EXECUTE FUNCTION weave_secret_decision_guard();

CREATE FUNCTION weave_secret_tenant_immutable() RETURNS trigger LANGUAGE plpgsql SET search_path=public,pg_temp AS $$
BEGIN
 IF TG_OP='DELETE' AND pg_trigger_depth()>1 AND NOT EXISTS(SELECT 1 FROM workspaces WHERE id=OLD.workspace_id) THEN RETURN OLD; END IF;
 RAISE EXCEPTION 'secret authority evidence is immutable' USING ERRCODE='restrict_violation';
END $$;
DO $$
DECLARE t text;
BEGIN
 FOREACH t IN ARRAY ARRAY['provider_secret_intents','provider_secret_assignments','provider_secret_approvals','provider_secret_withdrawals'] LOOP
  EXECUTE format('CREATE TRIGGER secret_tenant_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION weave_secret_tenant_immutable()',t);
  EXECUTE format('CREATE TRIGGER secret_tenant_no_truncate BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION weave_secret_tenant_immutable()',t);
 END LOOP;
END $$;
REVOKE ALL ON FUNCTION weave_secret_root_guard(),weave_secret_root_claimed(),weave_secret_intent_claim(),weave_secret_decision_guard(),weave_secret_tenant_immutable() FROM PUBLIC;
COMMENT ON TABLE provider_secret_reservations IS 'Permanent minimal nonreuse inventory. Reserved intent precedes external creation; consumed assignment does not itself approve cloud ownership.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Routine downgrade must never free reserved names, even after tenant deletion.
DO $$ BEGIN
 -- row_security=off does not bypass RLS: filtered callers ERROR instead of
 -- mistaking hidden inventory for an empty ledger. A trustworthy full-inventory
 -- migration capability is required even for verifying an empty downgrade.
 PERFORM set_config('row_security','off',true);
 IF EXISTS(SELECT 1 FROM provider_secret_reservations) THEN
  RAISE EXCEPTION 'populated secret ledger downgrade refused' USING ERRCODE='restrict_violation';
 END IF;
END $$;
DROP TABLE provider_secret_withdrawals,provider_secret_approvals,provider_secret_assignments,provider_secret_intents,provider_secret_reservations;
DROP FUNCTION weave_secret_tenant_immutable(),weave_secret_decision_guard(),weave_secret_intent_claim(),weave_secret_root_claimed(),weave_secret_root_guard();
REVOKE EXECUTE ON FUNCTION weave_current_workspace_id() FROM weave_secret_approval_reader,weave_secret_approval_writer;
REVOKE USAGE ON SCHEMA public FROM weave_secret_approval_reader,weave_secret_approval_writer;
-- Capability roles remain: they are cluster-wide, and other DBs may use them.
-- No login/membership was granted. Dropped-table grants disappear with tables.
-- +goose StatementEnd
