package api

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/backends/prom"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/hostedrunner"
	"github.com/buildbuddy-io/buildbuddy/proto/workflow"
	"github.com/buildbuddy-io/buildbuddy/server/build_event_protocol/build_event_handler"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/eventlog"
	"github.com/buildbuddy-io/buildbuddy/server/http/protolet"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/real_environment"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/tables"
	"github.com/buildbuddy-io/buildbuddy/server/util/capabilities"
	"github.com/buildbuddy-io/buildbuddy/server/util/db"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/perms"
	"github.com/buildbuddy-io/buildbuddy/server/util/prefix"
	"github.com/buildbuddy-io/buildbuddy/server/util/proto"
	"github.com/buildbuddy-io/buildbuddy/server/util/query_builder"
	"github.com/buildbuddy-io/buildbuddy/server/util/role"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/protobuf/types/known/timestamppb"

	api_common "github.com/buildbuddy-io/buildbuddy/server/api/common"
	requestcontext "github.com/buildbuddy-io/buildbuddy/server/util/request_context"

	apipb "github.com/buildbuddy-io/buildbuddy/proto/api/v1"
	bespb "github.com/buildbuddy-io/buildbuddy/proto/build_event_stream"
	cappb "github.com/buildbuddy-io/buildbuddy/proto/capability"
	elpb "github.com/buildbuddy-io/buildbuddy/proto/eventlog"
	gitpb "github.com/buildbuddy-io/buildbuddy/proto/git"
	inpb "github.com/buildbuddy-io/buildbuddy/proto/invocation"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	rspb "github.com/buildbuddy-io/buildbuddy/proto/resource"
	rnpb "github.com/buildbuddy-io/buildbuddy/proto/runner"
)

var (
	enableAPI            = flag.Bool("api.enable_api", true, "Whether or not to enable the BuildBuddy API.")
	enableCache          = flag.Bool("api.enable_cache", false, "Whether or not to enable the API cache.")
	enableCacheDeleteAPI = flag.Bool("enable_cache_delete_api", false, "If true, enable access to cache delete API.")
	enableMetricsAPI     = flag.Bool("api.enable_metrics_api", false, "If true, enable access to metrics API.")
)

type APIServer struct {
	env environment.Env
}

func Register(env *real_environment.RealEnv) error {
	if *enableAPI {
		env.SetAPIService(NewAPIServer(env))
	}
	return nil
}

func NewAPIServer(env environment.Env) *APIServer {
	return &APIServer{
		env: env,
	}
}

func (s *APIServer) authorizeWrites(ctx context.Context) error {
	canWrite, err := capabilities.IsGranted(ctx, s.env.GetAuthenticator(), cappb.Capability_CACHE_WRITE)
	if err != nil {
		return err
	}
	if !canWrite {
		return status.PermissionDeniedError("You do not have permission to perform this action.")
	}
	return nil
}

func (s *APIServer) GetInvocation(ctx context.Context, req *apipb.GetInvocationRequest) (*apipb.GetInvocationResponse, error) {
	user, err := s.env.GetAuthenticator().AuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}

	if req.GetSelector().GetInvocationId() == "" && req.GetSelector().GetCommitSha() == "" {
		return nil, status.InvalidArgumentErrorf("InvocationSelector must contain a valid invocation_id or commit_sha")
	}

	q := query_builder.NewQuery(`SELECT * FROM "Invocations"`)
	q = q.AddWhereClause(`group_id = ?`, user.GetGroupID())
	if req.GetSelector().GetInvocationId() != "" {
		q = q.AddWhereClause(`invocation_id = ?`, req.GetSelector().GetInvocationId())
	}
	if req.GetSelector().GetCommitSha() != "" {
		q = q.AddWhereClause(`commit_sha = ?`, req.GetSelector().GetCommitSha())
	}
	if err := perms.AddPermissionsCheckToQuery(ctx, s.env, q); err != nil {
		return nil, err
	}
	queryStr, args := q.Build()

	rq := s.env.GetDBHandle().NewQuery(ctx, "api_server_get_invocations").Raw(queryStr, args...)

	invocations := []*apipb.Invocation{}
	err = db.ScanEach(rq, func(ctx context.Context, ti *tables.Invocation) error {
		apiInvocation := &apipb.Invocation{
			Id: &apipb.Invocation_Id{
				InvocationId: ti.InvocationID,
			},
			Success:          ti.Success,
			User:             ti.User,
			DurationUsec:     ti.DurationUsec,
			Host:             ti.Host,
			Command:          ti.Command,
			Pattern:          ti.Pattern,
			ActionCount:      ti.ActionCount,
			CreatedAtUsec:    ti.CreatedAtUsec,
			UpdatedAtUsec:    ti.UpdatedAtUsec,
			RepoUrl:          ti.RepoURL,
			BranchName:       ti.BranchName,
			CommitSha:        ti.CommitSHA,
			Role:             ti.Role,
			BazelExitCode:    ti.BazelExitCode,
			InvocationStatus: apipb.InvocationStatus(ti.InvocationStatus),
		}

		invocations = append(invocations, apiInvocation)
		return nil
	})
	if err != nil {
		return nil, err
	}

	if req.IncludeMetadata || req.IncludeArtifacts {
		for _, i := range invocations {
			_, err := build_event_handler.LookupInvocationWithCallback(ctx, s.env, i.Id.InvocationId, func(event *inpb.InvocationEvent) error {
				switch p := event.GetBuildEvent().GetPayload().(type) {
				case *bespb.BuildEvent_BuildMetadata:
					if req.IncludeMetadata {
						for k, v := range p.BuildMetadata.GetMetadata() {
							i.BuildMetadata = append(i.BuildMetadata, &apipb.InvocationMetadata{
								Key:   k,
								Value: v,
							})
						}
					}
				case *bespb.BuildEvent_WorkspaceStatus:
					if req.IncludeMetadata {
						for _, item := range p.WorkspaceStatus.GetItem() {
							i.WorkspaceStatus = append(i.WorkspaceStatus, &apipb.InvocationMetadata{
								Key:   item.Key,
								Value: item.Value,
							})
						}
					}
				case *bespb.BuildEvent_NamedSetOfFiles:
					if req.IncludeArtifacts {
						for _, file := range p.NamedSetOfFiles.GetFiles() {
							i.Artifacts = append(i.Artifacts, &apipb.File{
								Name: file.GetName(),
								Uri:  file.GetUri(),
							})
						}
					}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}

	return &apipb.GetInvocationResponse{
		Invocation: invocations,
	}, nil
}

func (s *APIServer) CacheEnabled() bool {
	return *enableCache
}

func (s *APIServer) redisCachedTarget(ctx context.Context, userInfo interfaces.UserInfo, iid, targetLabel string) (*apipb.Target, error) {
	if !s.CacheEnabled() || s.env.GetMetricsCollector() == nil {
		return nil, nil
	}

	if targetLabel == "" {
		return nil, nil
	}
	key := api_common.TargetLabelKey(userInfo.GetGroupID(), iid, targetLabel)
	blobs, err := s.env.GetMetricsCollector().GetAll(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(blobs) != 1 {
		return nil, nil
	}

	t := &apipb.Target{}
	if err := proto.Unmarshal([]byte(blobs[0]), t); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *APIServer) GetTarget(ctx context.Context, req *apipb.GetTargetRequest) (*apipb.GetTargetResponse, error) {
	userInfo, err := s.env.GetAuthenticator().AuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetSelector().GetInvocationId() == "" {
		return nil, status.InvalidArgumentErrorf("TargetSelector must contain a valid invocation_id")
	}
	iid := req.GetSelector().GetInvocationId()

	rsp := &apipb.GetTargetResponse{
		Target: make([]*apipb.Target, 0),
	}

	cacheKey := req.GetSelector().GetLabel()
	// Target ID is equal to the target label, so either can be used as a cache key.
	if targetId := req.GetSelector().GetTargetId(); targetId != "" {
		cacheKey = targetId
	}

	cachedTarget, err := s.redisCachedTarget(ctx, userInfo, iid, cacheKey)
	if err != nil {
		log.Debugf("redisCachedTarget err: %s", err)
	} else if cachedTarget != nil {
		if api_common.TargetMatchesSelector(cachedTarget, req.GetSelector()) {
			rsp.Target = append(rsp.Target, cachedTarget)
		}
	}
	if len(rsp.Target) > 0 {
		return rsp, nil
	}

	targetMap := api_common.NewTargetMap(req.GetSelector())
	_, err = build_event_handler.LookupInvocationWithCallback(ctx, s.env, req.GetSelector().GetInvocationId(), func(event *inpb.InvocationEvent) error {
		targetMap.ProcessEvent(req.GetSelector().GetInvocationId(), event.GetBuildEvent())
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &apipb.GetTargetResponse{
		// Collect all the map values into a slice.
		Target: slices.Collect(maps.Values(targetMap.Targets)),
	}, nil
}

func (s *APIServer) redisCachedActions(ctx context.Context, userInfo interfaces.UserInfo, iid, targetLabel string) ([]*apipb.Action, error) {
	if !s.CacheEnabled() || s.env.GetMetricsCollector() == nil {
		return nil, nil
	}

	if targetLabel == "" {
		return nil, nil
	}

	const limit = 100_000
	key := api_common.ActionLabelKey(userInfo.GetGroupID(), iid, targetLabel)
	serializedResults, err := s.env.GetMetricsCollector().ListRange(ctx, key, 0, limit-1)
	if err != nil {
		return nil, err
	}
	a := &apipb.Action{}
	actions := make([]*apipb.Action, 0)
	for _, serializedResult := range serializedResults {
		if err := proto.Unmarshal([]byte(serializedResult), a); err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	return actions, nil
}

func (s *APIServer) GetAction(ctx context.Context, req *apipb.GetActionRequest) (*apipb.GetActionResponse, error) {
	userInfo, err := s.env.GetAuthenticator().AuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}

	if req.GetSelector().GetInvocationId() == "" {
		return nil, status.InvalidArgumentErrorf("ActionSelector must contain a valid invocation_id")
	}
	iid := req.GetSelector().GetInvocationId()
	rsp := &apipb.GetActionResponse{
		Action: make([]*apipb.Action, 0),
	}

	cacheKey := req.GetSelector().GetTargetLabel()
	// Target ID is equal to the target label, so either can be used as a cache key.
	if targetId := req.GetSelector().GetTargetId(); targetId != "" {
		cacheKey = targetId
	}

	cachedActions, err := s.redisCachedActions(ctx, userInfo, iid, cacheKey)
	if err != nil {
		log.Debugf("redisCachedAction err: %s", err)
	}
	for _, action := range cachedActions {
		if action != nil && actionMatchesActionSelector(action, req.GetSelector()) {
			rsp.Action = append(rsp.Action, action)
		}
	}
	if len(rsp.Action) > 0 {
		return rsp, nil
	}

	_, err = build_event_handler.LookupInvocationWithCallback(ctx, s.env, iid, func(event *inpb.InvocationEvent) error {
		action := &apipb.Action{
			Id: &apipb.Action_Id{
				InvocationId: iid,
			},
		}
		action = api_common.FillActionFromBuildEvent(event.GetBuildEvent(), action)

		// Filter to only selected actions.
		if action != nil && actionMatchesActionSelector(action, req.GetSelector()) {
			action = api_common.FillActionOutputFilesFromBuildEvent(event.GetBuildEvent(), action)
			rsp.Action = append(rsp.Action, action)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return rsp, nil
}

func (s *APIServer) GetLog(ctx context.Context, req *apipb.GetLogRequest) (*apipb.GetLogResponse, error) {
	// Check whether the user is authenticated. No need for the returned user
	// here, because user filters will be applied by LookupInvocation.
	if _, err := s.env.GetAuthenticator().AuthenticatedUser(ctx); err != nil {
		return nil, err
	}

	if req.GetSelector().GetInvocationId() == "" {
		return nil, status.InvalidArgumentErrorf("LogSelector must contain a valid invocation_id")
	}

	chunkReq := &elpb.GetEventLogChunkRequest{
		InvocationId: req.GetSelector().GetInvocationId(),
		ChunkId:      req.GetPageToken(),
	}

	resp, err := eventlog.GetEventLogChunk(ctx, s.env, chunkReq)
	if err != nil {
		log.Errorf("Encountered error getting event log chunk: %s\nRequest: %s", err, chunkReq)
		return nil, err
	}

	return &apipb.GetLogResponse{
		Log: &apipb.Log{
			Contents: string(resp.GetBuffer()),
		},
		NextPageToken: resp.GetNextChunkId(),
	}, nil
}

type getFileWriter struct {
	s apipb.ApiService_GetFileServer
}

func (gfs *getFileWriter) Write(data []byte) (int, error) {
	err := gfs.s.Send(&apipb.GetFileResponse{
		Data: data,
	})
	return len(data), err
}

func (s *APIServer) GetFile(req *apipb.GetFileRequest, server apipb.ApiService_GetFileServer) error {
	ctx := server.Context()
	if _, err := s.env.GetAuthenticator().AuthenticatedUser(ctx); err != nil {
		return err
	}

	parsedURL, err := url.Parse(req.GetUri())
	if err != nil {
		return status.InvalidArgumentErrorf("Invalid URL")
	}

	writer := &getFileWriter{s: server}

	return s.env.GetPooledByteStreamClient().StreamBytestreamFile(ctx, parsedURL, writer)
}

func (s *APIServer) DeleteFile(ctx context.Context, req *apipb.DeleteFileRequest) (*apipb.DeleteFileResponse, error) {
	if !*enableCacheDeleteAPI {
		return nil, status.PermissionDeniedError("DeleteFile API not enabled")
	}

	ctx, err := prefix.AttachUserPrefixToContext(ctx, s.env.GetAuthenticator())
	if err != nil {
		return nil, err
	}

	if _, err = s.env.GetAuthenticator().AuthenticatedUser(ctx); err != nil {
		return nil, err
	}
	if err = s.authorizeWrites(ctx); err != nil {
		return nil, err
	}

	parsedURL, err := url.Parse(req.GetUri())
	if err != nil {
		return nil, status.InvalidArgumentErrorf("Invalid URL")
	}
	urlStr := strings.TrimPrefix(parsedURL.RequestURI(), "/")

	var resourceName *rspb.ResourceName

	parsedACRN, err := digest.ParseActionCacheResourceName(urlStr)
	if err == nil {
		resourceName = digest.NewResourceName(parsedACRN.GetDigest(), parsedACRN.GetInstanceName(), rspb.CacheType_AC, parsedACRN.GetDigestFunction()).ToProto()
	} else {
		parsedCASRN, err := digest.ParseDownloadResourceName(urlStr)
		if err != nil {
			return nil, status.InvalidArgumentErrorf("Invalid URL. Only actioncache and CAS URIs supported.")
		}
		resourceName = digest.NewResourceName(parsedCASRN.GetDigest(), parsedCASRN.GetInstanceName(), rspb.CacheType_CAS, parsedCASRN.GetDigestFunction()).ToProto()
	}

	err = s.env.GetCache().Delete(ctx, resourceName)
	if err != nil && !status.IsNotFoundError(err) {
		return nil, err
	}

	return &apipb.DeleteFileResponse{}, nil
}

func (s *APIServer) GetFileHandler() http.Handler {
	return http.HandlerFunc(s.handleGetFileRequest)
}

// Handle streaming http GetFile request since protolet doesn't handle streaming rpcs yet.
func (s *APIServer) handleGetFileRequest(w http.ResponseWriter, r *http.Request) {
	if _, err := s.env.GetAuthenticator().AuthenticatedUser(r.Context()); err != nil {
		http.Error(w, "Invalid API key", http.StatusUnauthorized)
		return
	}

	req := apipb.GetFileRequest{}
	protolet.ReadRequestToProto(r, &req)

	parsedURL, err := url.Parse(req.GetUri())
	if err != nil {
		http.Error(w, "Invalid URI", http.StatusBadRequest)
		return
	}

	err = s.env.GetPooledByteStreamClient().StreamBytestreamFile(r.Context(), parsedURL, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
	}
}

func (s *APIServer) GetMetricsHandler() http.Handler {
	return http.HandlerFunc(s.handleGetMetricsRequest)
}

func (s *APIServer) handleGetMetricsRequest(w http.ResponseWriter, r *http.Request) {
	if !*enableMetricsAPI {
		http.Error(w, "API not enabled", http.StatusNotImplemented)
		return
	}
	userInfo, err := s.env.GetAuthenticator().AuthenticatedUser(r.Context())
	if err != nil {
		http.Error(w, "Invalid API key", http.StatusUnauthorized)
		return
	}
	if userInfo.GetGroupID() == "" {
		http.Error(w, "Invalid API key", http.StatusUnauthorized)
		return
	}
	// query prometheus
	reg, err := prom.NewRegistry(s.env, userInfo.GetGroupID())
	if err != nil {
		http.Error(w, "unable to get registry", http.StatusInternalServerError)
		return
	}
	opts := promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
		Registry:      reg,
		// Gzip is handlered by intercepters already.
		DisableCompression: true,
	}
	handler := promhttp.HandlerFor(reg, opts)
	handler.ServeHTTP(w, r)
}

// Returns true if a selector doesn't specify a particular id or matches the target's ID
func actionMatchesActionSelector(action *apipb.Action, selector *apipb.ActionSelector) bool {
	return (selector.TargetId == "" || selector.TargetId == action.GetId().TargetId) &&
		(selector.TargetLabel == "" || selector.TargetLabel == action.GetTargetLabel()) &&
		(selector.ConfigurationId == "" || selector.ConfigurationId == action.GetId().ConfigurationId) &&
		(selector.ActionId == "" || selector.ActionId == action.GetId().ActionId)
}

func (s *APIServer) ExecuteWorkflow(ctx context.Context, req *apipb.ExecuteWorkflowRequest) (*apipb.ExecuteWorkflowResponse, error) {
	user, err := s.env.GetAuthenticator().AuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	if user.GetGroupID() == "" {
		return nil, status.InternalErrorf("authenticated user's group ID is empty")
	}

	wfs := s.env.GetWorkflowService()
	requestCtx := requestcontext.ProtoRequestContextFromContext(ctx)

	wfID := wfs.GetLegacyWorkflowIDForGitRepository(user.GetGroupID(), req.GetRepoUrl())
	branch := req.GetBranch()
	if branch == "" && req.GetCommitSha() == "" {
		// For backwards compatibility, set branch from `ref` if neither `branch`
		// or `commit_sha` are set
		branch = req.GetRef()
	}
	r := &workflow.ExecuteWorkflowRequest{
		RequestContext: requestCtx,
		WorkflowId:     wfID,
		ActionNames:    req.GetActionNames(),
		PushedRepoUrl:  req.GetRepoUrl(),
		PushedBranch:   branch,
		CommitSha:      req.GetCommitSha(),
		Visibility:     req.GetVisibility(),
		Async:          req.GetAsync(),
		Env:            req.GetEnv(),
		DisableRetry:   req.GetDisableRetry(),
	}
	rsp, err := wfs.ExecuteWorkflow(ctx, r)
	if err != nil {
		return nil, err
	}

	actionStatuses := make([]*apipb.ExecuteWorkflowResponse_ActionStatus, len(rsp.GetActionStatuses()))
	for i, as := range rsp.GetActionStatuses() {
		actionStatuses[i] = &apipb.ExecuteWorkflowResponse_ActionStatus{
			ActionName:   as.ActionName,
			InvocationId: as.InvocationId,
			Status:       as.Status,
		}
	}
	return &apipb.ExecuteWorkflowResponse{
		ActionStatuses: actionStatuses,
	}, nil
}

func (s *APIServer) Run(ctx context.Context, req *apipb.RunRequest) (*apipb.RunResponse, error) {
	r, err := hostedrunner.New(s.env)
	if err != nil {
		return nil, err
	}

	steps := make([]*rnpb.Step, 0, len(req.GetSteps()))
	for _, s := range req.GetSteps() {
		steps = append(steps, &rnpb.Step{Run: s.Run})
	}
	execProps := make([]*repb.Platform_Property, 0, len(req.GetPlatformProperties()))
	for k, v := range req.GetPlatformProperties() {
		execProps = append(execProps, &repb.Platform_Property{
			Name:  k,
			Value: v,
		})
	}

	rsp, err := r.Run(ctx, &rnpb.RunRequest{
		GitRepo: &gitpb.GitRepo{RepoUrl: req.GetRepo()},
		RepoState: &gitpb.RepoState{
			CommitSha: req.GetCommitSha(),
			Branch:    req.GetBranch(),
			Patch:     req.GetPatches(),
		},
		Steps:          steps,
		Async:          req.GetAsync(),
		Env:            req.GetEnv(),
		Timeout:        req.GetTimeout(),
		ExecProperties: execProps,
		RemoteHeaders:  req.GetRemoteHeaders(),
		RunRemotely:    true,
		RunnerFlags:    []string{fmt.Sprintf("--skip_auto_checkout=%v", req.GetSkipAutoCheckout())},
	})
	if err != nil {
		return nil, err
	}
	return &apipb.RunResponse{InvocationId: rsp.InvocationId}, nil
}

func (s *APIServer) CreateUserApiKey(ctx context.Context, req *apipb.CreateUserApiKeyRequest) (*apipb.CreateUserApiKeyResponse, error) {
	u, err := s.env.GetAuthenticator().AuthenticatedUser(ctx)
	if err != nil {
		return nil, err
	}
	authdb := s.env.GetAuthDB()
	if authdb == nil {
		return nil, status.UnimplementedError("not implemented")
	}
	userdb := s.env.GetUserDB()
	if userdb == nil {
		return nil, status.UnimplementedError("not implemented")
	}

	// Get user's role-based capabilities within the group.
	reqUser, err := userdb.GetUserByIDWithoutAuthCheck(ctx, req.GetUserId())
	if err != nil {
		return nil, err
	}
	var groupRole *tables.GroupRole
	for _, g := range reqUser.Groups {
		if g.Group.GroupID == u.GetGroupID() {
			groupRole = g
			break
		}
	}
	if groupRole == nil {
		return nil, status.PermissionDeniedError("permission denied")
	}
	roleBasedCapabilities, err := role.ToCapabilities(role.Role(groupRole.Role))
	if err != nil {
		return nil, err
	}
	// Apply the user API key capabilities mask.
	roleBasedCapabilities = capabilities.ApplyMask(roleBasedCapabilities, capabilities.UserAPIKeyCapabilitiesMask)

	// Note: authdb performs additional authentication checks, such as making
	// sure the authenticated user has ORG_ADMIN capability if needed.
	apiKey, err := authdb.CreateUserAPIKey(
		ctx, u.GetGroupID(), req.GetUserId(), req.GetLabel(),
		roleBasedCapabilities, req.GetExpiresIn().AsDuration(),
	)
	if err != nil {
		return nil, err
	}
	rsp := &apipb.CreateUserApiKeyResponse{
		ApiKey: &apipb.ApiKey{
			ApiKeyId: apiKey.APIKeyID,
			Value:    apiKey.Value,
			Label:    apiKey.Label,
		},
	}
	if apiKey.ExpiryUsec != 0 {
		rsp.ApiKey.ExpirationTimestamp = timestamppb.New(time.UnixMicro(apiKey.ExpiryUsec))
	}
	return rsp, nil
}
