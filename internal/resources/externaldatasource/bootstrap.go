package externaldatasource

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ydb-platform/terraform-provider-ydb/internal/helpers"
)

type mutationOutcome uint8

const (
	notSubmitted mutationOutcome = iota
	terminalSuccess
	terminalFailure
	unknownOutcome
)

type mutationResult struct {
	outcome mutationOutcome
	err     error
}
type bootstrapResult struct {
	mutationResult
	path       string
	owned      bool
	cleanupErr error
}

var errCallerTokenRequired = errors.New("caller IAM token required")

// safeStatus deliberately excludes server issue strings, RPC messages and query
// parameter dumps: those may echo a credential, even though SQL is parameterized.
func safeStatus(err error) string {
	if err == nil {
		return "OK"
	}
	if errors.Is(err, errCallerTokenRequired) {
		return errCallerTokenRequired.Error()
	}
	if e := ydb.OperationError(err); e != nil {
		return fmt.Sprintf("YDB status %d", e.Code())
	}
	if e := ydb.TransportError(err); e != nil {
		return fmt.Sprintf("transport status %d", e.Code())
	}
	if s, ok := status.FromError(err); ok {
		return "RPC " + s.Code().String()
	}
	if errors.Is(err, context.Canceled) {
		return "context cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline exceeded"
	}
	return "request failed (details omitted to protect credentials)"
}

func classifyMutation(err error) mutationResult {
	if err == nil {
		return mutationResult{outcome: terminalSuccess}
	}
	// A terminal rejection is different from a lost/cancelled response. In
	// particular, TIMEOUT/CANCELLED/UNAVAILABLE/UNDETERMINED cannot prove rollback.
	if ydb.IsOperationError(err, Ydb.StatusIds_BAD_REQUEST, Ydb.StatusIds_UNAUTHORIZED, Ydb.StatusIds_PRECONDITION_FAILED, Ydb.StatusIds_ALREADY_EXISTS, Ydb.StatusIds_NOT_FOUND, Ydb.StatusIds_SCHEME_ERROR, Ydb.StatusIds_UNSUPPORTED) {
		return mutationResult{outcome: terminalFailure, err: err}
	}
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument, codes.AlreadyExists, codes.NotFound, codes.FailedPrecondition, codes.Unimplemented:
		return mutationResult{outcome: terminalFailure, err: err}
	}
	return mutationResult{outcome: unknownOutcome, err: err}
}

func execMutation(ctx context.Context, b backend, q string, opts ...query.ExecuteOption) mutationResult {
	if err := ctx.Err(); err != nil {
		return mutationResult{outcome: notSubmitted, err: err}
	}
	return classifyMutation(b.Exec(ctx, q, opts...))
}

func isCollision(err error) bool {
	return ydb.IsOperationErrorAlreadyExistsError(err) || status.Code(err) == codes.AlreadyExists
}

func (h *Handler) boundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := h.cleanupTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (h *Handler) close(ctx context.Context, b backend) {
	closeCtx, cancel := h.boundedContext(ctx)
	defer cancel()
	_ = b.Close(closeCtx)
}

func (h *Handler) freshBackend(ctx context.Context, endpoint string) (backend, error) {
	creds := h.authCreds
	if h.callerAuth != nil {
		var err error
		creds, err = h.callerAuth(ctx)
		if err != nil {
			return nil, err
		}
	}
	if h.openBackend != nil {
		return h.openBackend(ctx, endpoint, creds)
	}
	return openDriverBackend(ctx, endpoint, creds)
}

func (h *Handler) cleanup(ctx context.Context, endpoint, p string) error {
	cleanCtx, cancel := h.boundedContext(ctx)
	defer cancel()
	b, err := h.freshBackend(cleanCtx, endpoint)
	if err != nil {
		return err
	}
	defer h.close(cleanCtx, b)
	err = b.Exec(cleanCtx, "DROP SECRET `"+helpers.EscapeYQLIdentifier(p)+"`")
	if err == nil {
		return nil
	}
	// Lost DROP response and NotFound are safe only after exact absence is
	// confirmed by a successful parent listing. Never infer it from SCHEME_ERROR.
	absent, probeErr := b.PathAbsent(cleanCtx, p)
	if probeErr == nil && absent {
		return nil
	}
	return err
}

func (h *Handler) bootstrap(ctx context.Context, endpoint, root string, b backend, mutation func(string) mutationResult) bootstrapResult {
	if err := ctx.Err(); err != nil {
		return bootstrapResult{mutationResult: mutationResult{outcome: notSubmitted, err: err}}
	}
	creds := h.authCreds
	if h.callerAuth != nil {
		var err error
		creds, err = h.callerAuth(ctx)
		if err != nil {
			return bootstrapResult{mutationResult: mutationResult{outcome: notSubmitted, err: err}}
		}
	}
	if creds.Token == "" {
		return bootstrapResult{mutationResult: mutationResult{outcome: notSubmitted, err: errCallerTokenRequired}}
	}
	var p string
	for attempt := 0; attempt < 5; attempt++ {
		name := "__tf_iam_bootstrap_" + uuid.NewString()
		if h.uniqueName != nil {
			name = h.uniqueName()
		}
		p = path.Join(root, name)
		q := "DECLARE $value AS Utf8; CREATE SECRET `" + helpers.EscapeYQLIdentifier(p) + "` WITH (value = $value)"
		result := execMutation(ctx, b, q, query.WithParameters(ydb.ParamsBuilder().Param("$value").Text(creds.Token).Build()))
		if result.outcome == terminalSuccess {
			break
		}
		if result.outcome == terminalFailure && isCollision(result.err) {
			if attempt < 4 {
				continue
			}
			p = ""
		}
		return bootstrapResult{mutationResult: result, path: p}
	}
	result := bootstrapResult{mutationResult: mutation(p), path: p, owned: true}
	if result.outcome == unknownOutcome {
		return result
	}
	result.cleanupErr = h.cleanup(ctx, endpoint, p)
	return result
}

func (h *Handler) recoverCleanup(ctx context.Context, d *schema.ResourceData, endpoint string) diag.Diagnostics {
	p, _ := d.Get("pending_cleanup_path").(string)
	uncertain, _ := d.Get("pending_iam_mutation").(string)
	if uncertain != "" {
		if p != "" {
			return diag.Errorf("IAM mutation outcome remains unknown; confirmed-owned bootstrap secret %q is preserved. Verify the server operation has stopped before manual recovery; do not drop a possibly in-use secret. See IAM bootstrap recovery documentation", p)
		}
		return diag.Errorf("IAM mutation outcome remains unknown. Verify the server operation has stopped before manual recovery. See IAM bootstrap recovery documentation")
	}
	if p == "" {
		return nil
	}
	if err := h.cleanup(ctx, endpoint, p); err != nil {
		return diag.Errorf("cannot clean confirmed-owned bootstrap secret %q: %s; EDS identity and pending cleanup are retained", p, safeStatus(err))
	}
	return diag.FromErr(d.Set("pending_cleanup_path", ""))
}

func (h *Handler) reconcileUnknown(ctx context.Context, endpoint, fullPath string) {
	probeCtx, cancel := h.boundedContext(ctx)
	defer cancel()
	b, err := h.freshBackend(probeCtx, endpoint)
	if err != nil {
		return
	}
	defer h.close(probeCtx, b)
	// Query streaming has no recoverable operation ID. Describe is useful bounded
	// reconciliation, but neither equality nor absence proves a request has stopped.
	// In particular it never establishes ownership of an uncertain strict Create.
	_, _ = b.Describe(probeCtx, fullPath)
}

func bootstrapDiagnostics(result bootstrapResult) diag.Diagnostics {
	if result.outcome == terminalSuccess {
		return nil
	}
	detail := "EDS mutation failed: " + safeStatus(result.err)
	if result.outcome == unknownOutcome {
		detail = "EDS mutation outcome is unknown: " + safeStatus(result.err) + ". Reconcile server-side execution; an equal Describe does not establish Create ownership. Import only after verifying ownership"
	}
	if result.path != "" && !result.owned {
		detail += ". Secret CREATE was not acknowledged; ownership of path " + fmt.Sprintf("%q", result.path) + " is unconfirmed. Do not delete it without independent ownership evidence"
	}
	if result.owned && result.outcome == unknownOutcome {
		detail += ". Confirmed-owned bootstrap secret " + fmt.Sprintf("%q", result.path) + " is preserved while the mutation may still be running"
	}
	if result.owned && result.cleanupErr != nil {
		detail += ". Cleanup also failed (" + safeStatus(result.cleanupErr) + "); confirmed-owned orphan " + fmt.Sprintf("%q", result.path) + " requires manual DROP SECRET after verifying terminal mutation. No EDS ownership was synthesized"
	}
	return diag.Errorf("%s", detail)
}
