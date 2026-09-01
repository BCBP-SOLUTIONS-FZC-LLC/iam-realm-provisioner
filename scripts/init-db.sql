-- Local-dev bootstrap for the `provisioner` database and its two DB roles
-- (S4.5, S10.1). Mirrors iam-org-membership's scripts/init-db.sql
-- convention - production provisions these via Terraform; the domain
-- migration (000000_initial_schema.up.sql) only ALTERs/GRANTs, it never
-- CREATE ROLEs.
--
-- realm_provisioner_app       - runtime app role. NOBYPASSRLS.
-- realm_provisioner_migrator  - migrations + reconciler role. BYPASSRLS.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'realm_provisioner_app') THEN
        CREATE ROLE realm_provisioner_app LOGIN PASSWORD 'realm_provisioner_app_dev_password' NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'realm_provisioner_migrator') THEN
        CREATE ROLE realm_provisioner_migrator LOGIN PASSWORD 'realm_provisioner_migrator_dev_password' BYPASSRLS;
    END IF;
END
$$;

SELECT 'CREATE DATABASE provisioner OWNER realm_provisioner_migrator'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'provisioner')\gexec

GRANT ALL PRIVILEGES ON DATABASE provisioner TO realm_provisioner_migrator;
GRANT CONNECT ON DATABASE provisioner TO realm_provisioner_app;

\c provisioner

GRANT USAGE, CREATE ON SCHEMA public TO realm_provisioner_migrator;
GRANT USAGE ON SCHEMA public TO realm_provisioner_app;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
