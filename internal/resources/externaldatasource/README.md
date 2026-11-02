# ydb_external_data_source resource

`ydb_external_data_source` resource is used to manage YDB [external data source](https://ydb.tech/docs/ru/concepts/datamodel/external_data_source) entity.

Configuration changes are applied in place via `CREATE OR REPLACE EXTERNAL DATA SOURCE`. Only `connection_string`, `path`, and `source_type` require resource recreation.

## Example

```tf
resource "ydb_external_data_source" "object_storage" {
    path              = "path/to/external_data_source"
    connection_string = "grpc://localhost:2136/?database=/local"

    source_type = "ObjectStorage"
    location    = "https://storage.yandexcloud.net/my_bucket_name/"
    auth_method = "NONE"
}
```

```tf
resource "ydb_external_data_source" "clickhouse" {
    path              = "path/to/clickhouse_source"
    connection_string = "grpc://localhost:2136/?database=/local"

    source_type          = "ClickHouse"
    location             = "clickhouse-host:9000"
    auth_method          = "BASIC"
    login                = "user"
    password_secret_name = "my_password_secret"
    database_name        = "default"
    protocol             = "NATIVE"
    use_tls              = true
}
```

```tf
resource "ydb_external_data_source" "postgresql" {
    path              = "path/to/pg_source"
    connection_string = "grpc://localhost:2136/?database=/local"

    source_type                  = "PostgreSQL"
    location                     = "localhost:6432"
    auth_method                  = "MDB_BASIC"
    service_account_id           = "sa-id-123"
    service_account_secret_path  = "sa_secret"
    login                        = "pguser"
    password_secret_path         = "pg_pass"
    database_name                = "mydb"
    mdb_cluster_id               = "c9q1234567890"
    use_tls                      = true
}
```

```tf
resource "ydb_external_data_source" "s3_aws" {
    path              = "path/to/s3_source"
    connection_string = "grpc://localhost:2136/?database=/local"

    source_type                        = "ObjectStorage"
    location                           = "https://storage.yandexcloud.net/my_bucket_name/"
    auth_method                        = "AWS"
    aws_access_key_id_secret_path      = "aws_key_id"
    aws_secret_access_key_secret_path  = "aws_secret_key"
    aws_region                         = "ru-central-1"
}
```

## Argument Reference

### Required

- `connection_string` (String) - Database connection string, e.g. `grpc://localhost:2136/?database=/local`.
- `path` (String) - Path to the external data source within the database.
- `source_type` (String) - Type of external data source: `ObjectStorage`, `ClickHouse`, `PostgreSQL`.
- `location` (String) - Network address of the external data source (host:port or URL).

### Optional

- `auth_method` (String) - Authentication method. One of: `NONE`, `BASIC`, `MDB_BASIC`, `AWS`, `TOKEN`, `SERVICE_ACCOUNT`.
- `database_name` (String) - Database name in the external source (for ClickHouse/PostgreSQL).
- `protocol` (String) - Communication protocol: `NATIVE`, `HTTP`.
- `use_tls` (Boolean) - Enable TLS for the external data source connection.
- `mdb_cluster_id` (String) - Managed Database cluster ID.

#### BASIC / MDB_BASIC auth parameters

- `login` (String) - Username.
- `password_secret_name` (String) - Secret name for the password.
- `password_secret_path` (String) - Secret path for the password.

> Specify either `password_secret_name` or `password_secret_path`, not both.

#### MDB_BASIC additional parameters

- `service_account_id` (String) - Service account ID.
- `service_account_secret_name` (String) - Secret name for the service account.
- `service_account_secret_path` (String) - Secret path for the service account.

#### SERVICE_ACCOUNT auth parameters

- `service_account_id` (String) - Service account ID.
- `service_account_secret_name` (String) - Secret name for the service account.
- `service_account_secret_path` (String) - Secret path for the service account.

#### AWS auth parameters

- `aws_access_key_id_secret_path` (String) - Secret path for AWS access key ID.
- `aws_secret_access_key_secret_path` (String) - Secret path for AWS secret access key.
- `aws_region` (String) - AWS region.

#### TOKEN auth parameters

- `token_secret_path` (String) - Secret path for the token.

## Validation

The provider validates `auth_method` parameters before sending requests to YDB:

- **Unsupported parameters** - fields belonging to a different auth method are rejected (e.g. `aws_region` with `AUTH_METHOD = "BASIC"`).
- **Required parameters** - mandatory fields for each auth method must be provided (e.g. `login` for `BASIC`).

## Data Source

`ydb_external_data_source` data source reads an existing external data source. Only `connection_string` and `path` are required; all other attributes are computed.

```tf
data "ydb_external_data_source" "example" {
    path              = "path/to/external_data_source"
    connection_string = "grpc://localhost:2136/?database=/local"
}
```

## Cloud IAM for a YDB target

Use `auth_method = "IAM"` with `source_type = "Ydb"` and the runtime
`service_account_id`. The provider caller must already have permission to use
that account, resolve the target, and create secrets and EDS objects in the owner
database. Configure the target database's delegation and runtime roles separately.
The caller token bootstraps the operation; it is not the runtime SA credential.

```tf
resource "ydb_external_data_source" "cloud_target" {
    connection_string  = "grpcs://streaming.example:2135/?database=/streaming"
    path               = "cloud_target"
    source_type        = "Ydb"
    location           = "target.example:2135"
    database_id        = "target-database-id"
    auth_method        = "IAM"
    service_account_id = "runtime-service-account-id"
    iam_bootstrap_mode = "CALLER_TOKEN"
    use_tls            = true
}
```

`connection_string` names the owner/streaming database; `database_id`,
`database_name`, and `location` identify the target. Supply a target endpoint
reachable by the YDB runtime (including the appropriate `u-lb` endpoint in
installations that require it). The generic provider does not rewrite endpoints.
`resource_id` is Optional+Computed and means the **target cloud ID**, not either
database ID. Usually omit it so YDB resolves it. Changing the target invalidates
an implicitly resolved cloud ID; an explicitly configured ID remains authoritative.

New IAM resources default to `CALLER_TOKEN`. On every Create/Update, the provider
gets fresh caller credentials from its existing callback, strictly creates a
random root secret, passes its path to ordinary EDS DDL, then awaits secret
removal. The value is a Utf8 `$value` query parameter and is never placed in EDS
resource state, SQL literals, or diagnostics. A fixed provider token still expires:
credential renewal is the caller callback's responsibility. Go strings do not
provide a secure memory-zeroization guarantee.

`initial_token_secret_path` is Optional+Computed. In `CALLER_TOKEN` mode, omit it
from configuration: its state value is Describe metadata for a temporary object
that is normally already absent. Refresh and a no-op plan neither recreate that
secret nor request a new bootstrap token. Reads may authenticate their ordinary
YDB requests with the provider callback.

Use `iam_bootstrap_mode = "EXISTING_SECRET"` and a configured
`initial_token_secret_path` to use an externally managed legacy secret. The
provider never alters or deletes it. Relative paths and their equivalent absolute
owner paths compare equally. Import defaults safely to `EXISTING_SECRET` and
reads successfully even if the observed initial secret was removed. Before an
update, either explicitly select `CALLER_TOKEN` (and omit the path), or configure
a usable external secret path; copying an observed temporary path is insufficient.

IAM Create uses strict `CREATE EXTERNAL DATA SOURCE`; it never replaces an
existing object. IAM Update uses `CREATE OR REPLACE` without a preliminary DROP,
preserving dependent external tables. Existing non-IAM Create behavior remains
`CREATE OR REPLACE`.

### Cleanup and interrupted operations

Cleanup uses a separate ten-second context after terminal EDS success or failure,
including caller cancellation, and obtains fresh transport credentials. It removes
only an acknowledged, owned secret. A collision gets a new random name; the
colliding object is never altered or deleted. Absence after failed DROP is accepted
only after a successful listing confirms that exact path is absent.

After successful EDS mutation, failed cleanup returns an explicit **warning** and
persists `pending_cleanup_path`. This is deliberate: Terraform 1.5.7 taints a
successful Create when its handler returns an error, causing destructive replacement.
The warning retains the confirmed EDS without taint. The next refresh retries only
that exact path; persistent failure returns an error, keeps identity and pending
metadata, and blocks ordinary planning/apply. Restore secret DDL permissions or
connectivity and run `terraform apply` or `terraform apply -refresh-only`. Do not
clear pending metadata to silence the error. A failed read-back after acknowledged
IAM mutation likewise preserves identity with a refresh warning.

A deadline, transport loss, or process crash can leave an unknown outcome. The
provider makes a bounded Describe attempt but never treats an equal description
as proof of Create ownership. An unconfirmed secret CREATE path is reported as
**unconfirmed** and never automatically dropped. An acknowledged temporary secret
is preserved while EDS mutation may still run. Unknown Update retains the previous
EDS ID, `pending_cleanup_path`, and `pending_iam_mutation = "UNKNOWN"`; refresh
blocks cleanup until an operator reconciles server execution. Unknown/failed Create
never invents an EDS identity merely to retain cleanup state.

For an exact **confirmed-owned orphan** in diagnostics, first verify with the YDB
operator that the submitted EDS request has terminated. Then remove that exact
secret (`DROP SECRET` with proper identifier quoting), verify the intended EDS and
its ownership, and import it if appropriate. For an unknown Update, back up state,
finish exact-path manual recovery, then re-import the verified EDS to clear uncertain
metadata and explicitly restore `CALLER_TOKEN` configuration. Never drop an
unconfirmed path without independent ownership evidence. Never sweep secrets by
`__tf_iam_bootstrap_` prefix.

A hard process kill before Terraform receives state can leave an orphan whose path
was never checkpointed. A rerun uses a new name and cannot discover ownership of
that orphan automatically. Use server audit records and independent operation
ownership evidence for manual recovery. Token expiry limits the credential's useful
life but does not remove its secret object. Back up state before import or rollback.

### Provider compatibility and live verification

This feature adds provider fields and no YDB protocol/server changes. New IAM
configuration requires a provider release containing this schema. Before downgrading
providers, preserve backups and intentionally remove or transfer management of new
IAM resources; restore compatible configuration and state. An old provider is not
expected to accept arbitrary new IAM state. Legacy-only old→new→old state is
covered by the local `TestLocalTerraformYDBLegacyProviderUpgradeDowngrade` when
an explicit baseline binary is supplied. This changes only Go providers using
existing SQL/CP APIs: no YDB or YDB CP upgrade, rolling deployment, mixed-version
server exercise or server restart is required to deploy this feature.

The local tests exercise real Terraform 1.5.7 state/plan semantics, real SDK query
parameter wiring against fake gRPC, cancellation, unknown replies, and process kill.
They do not establish live cloud authorization or runtime behavior after secret DROP.
`TestAccYDBExternalDataSourceIAM` is a separately gated live DDL/no-op/import test:
set `TF_ACC=1`, `YDB_ACC_IAM_LIVE=1`, `YDB_ACC_CONNECTION_STRING`, `YDB_ACC_TOKEN`,
`YDB_ACC_IAM_TARGET_DATABASE_ID`, `YDB_ACC_IAM_SERVICE_ACCOUNT_ID`,
`YDB_ACC_IAM_TARGET_LOCATION`, and `YDB_ACC_IAM_TARGET_CLOUD_ID` in a dedicated
preconfigured environment. It checks the resolved cloud ID and secret absence.
It does not print or place the token in HCL. Verify the wrapper on the existing
deployment: delegation and EDS Create, temporary-secret DROP followed by a fresh
query, fresh-token Update and cleanup, and exact-pair Revoke/destroy. Include
least-privilege caller/runtime checks and provider state rollback. Runtime restart
or server rolling-deployment exercises are optional extra validation, not required
deployment steps or release gates; they require separate authorization.
