package content_addressable_storage_server_proxy

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/atime_updater"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/byte_stream_server_proxy"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/proxy_util"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/byte_stream_server"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/cachetools"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/content_addressable_storage_server"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/cas"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testenv"
	"github.com/buildbuddy-io/buildbuddy/server/util/authutil"
	"github.com/buildbuddy-io/buildbuddy/server/util/testing/flags"
	"github.com/buildbuddy-io/buildbuddy/server/util/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	bspb "google.golang.org/genproto/googleapis/bytestream"
)

const (
	fooDigest   = "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"
	foofDigest  = "ecebed81d223de4ccfbcf9cee4e19e1872165b8a142c2d6ee6fb1d29617d0e8e"
	barDigest   = "fcde2b2edba56bf408601fb721fe9b5c338d10ee429ea04fae5511b68fbf8fb9"
	barrDigest  = "8fa319f9b487d6ae32862c952d708b192a999b2f96bda081e8a49a0c3fb99265"
	barrrDigest = "39938f9489bc9b0f9d7308be111b90a615942ebc4530f0bf5c98e6083af29ee8"
	bazDigest   = "baa5a0964d3320fbc0c6a922140453c8513ea24ab8fd0577034804a967248096"
	quxDigest   = "21f58d27f827d295ffcd860c65045685e3baf1ad4506caa0140113b316647534"

	atimeUpdatePeriod = time.Minute
)

func requestCountingUnaryInterceptor(count *atomic.Int32) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		count.Add(1)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func requestCountingStreamInterceptor(count *atomic.Int32) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		count.Add(1)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

func runRemoteCASS(ctx context.Context, env *testenv.TestEnv, t testing.TB) (*grpc.ClientConn, *atomic.Int32, *atomic.Int32) {
	casServer, err := content_addressable_storage_server.NewContentAddressableStorageServer(env)
	require.NoError(t, err)
	bsServer, err := byte_stream_server.NewByteStreamServer(env)
	require.NoError(t, err)
	grpcServer, runFunc, lis := testenv.RegisterLocalGRPCServer(t, env)
	repb.RegisterContentAddressableStorageServer(grpcServer, casServer)
	bspb.RegisterByteStreamServer(grpcServer, bsServer)
	go runFunc()
	unaryRequestCounter := atomic.Int32{}
	streamRequestCounter := atomic.Int32{}
	conn, err := testenv.LocalGRPCConn(ctx, lis,
		grpc.WithUnaryInterceptor(requestCountingUnaryInterceptor(&unaryRequestCounter)),
		grpc.WithStreamInterceptor(requestCountingStreamInterceptor(&streamRequestCounter)))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn, &unaryRequestCounter, &streamRequestCounter
}

func runLocalCASS(ctx context.Context, env *testenv.TestEnv, t testing.TB) repb.ContentAddressableStorageServer {
	cas, err := content_addressable_storage_server.NewContentAddressableStorageServer(env)
	require.NoError(t, err)
	return cas
}

func runCASProxy(ctx context.Context, clientConn *grpc.ClientConn, env *testenv.TestEnv, t testing.TB) *grpc.ClientConn {
	env.SetByteStreamClient(bspb.NewByteStreamClient(clientConn))
	env.SetContentAddressableStorageClient(repb.NewContentAddressableStorageClient(clientConn))
	bss, err := byte_stream_server.NewByteStreamServer(env)
	require.NoError(t, err)
	env.SetLocalByteStreamServer(bss)
	cas, err := content_addressable_storage_server.NewContentAddressableStorageServer(env)
	require.NoError(t, err)
	env.SetLocalCASServer(cas)
	casServer, err := New(env)
	require.NoError(t, err)
	bsServer, err := byte_stream_server_proxy.New(env)
	require.NoError(t, err)
	grpcServer, runFunc, lis := testenv.RegisterLocalGRPCServer(t, env)
	repb.RegisterContentAddressableStorageServer(grpcServer, casServer)
	bspb.RegisterByteStreamServer(grpcServer, bsServer)
	go runFunc()
	conn, err := testenv.LocalGRPCConn(ctx, lis, grpc.WithDefaultCallOptions())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func digestProto(hash string, size int64) *repb.Digest {
	return &repb.Digest{Hash: hash, SizeBytes: size}
}

func findMissingBlobsRequest(digests []*repb.Digest) *repb.FindMissingBlobsRequest {
	return &repb.FindMissingBlobsRequest{
		BlobDigests:    digests,
		DigestFunction: repb.DigestFunction_SHA256,
	}
}

func readBlobsRequest(digests []*repb.Digest) *repb.BatchReadBlobsRequest {
	return &repb.BatchReadBlobsRequest{
		Digests:               digests,
		AcceptableCompressors: []repb.Compressor_Value{repb.Compressor_IDENTITY},
		DigestFunction:        repb.DigestFunction_SHA256,
	}
}

func updateBlobsRequest(blobs map[*repb.Digest]string) *repb.BatchUpdateBlobsRequest {
	request := repb.BatchUpdateBlobsRequest{DigestFunction: repb.DigestFunction_SHA256}
	for digest, data := range blobs {
		request.Requests = append(request.Requests, &repb.BatchUpdateBlobsRequest_Request{
			Digest: digest,
			Data:   []byte(data),
		})
	}
	return &request
}

func findMissing(ctx context.Context, client repb.ContentAddressableStorageClient, digests []*repb.Digest, missing []*repb.Digest, t *testing.T) {
	resp, err := client.FindMissingBlobs(ctx, findMissingBlobsRequest(digests))
	require.NoError(t, err)
	require.Equal(t, len(missing), len(resp.MissingBlobDigests))
	for i := range missing {
		require.Equal(t, missing[i].Hash, resp.MissingBlobDigests[i].Hash)
		require.Equal(t, missing[i].SizeBytes, resp.MissingBlobDigests[0].SizeBytes)
	}
}

func read(ctx context.Context, client repb.ContentAddressableStorageClient, digests []*repb.Digest, blobs map[string]string, t *testing.T) {
	resp, err := client.BatchReadBlobs(ctx, readBlobsRequest(digests))
	require.NoError(t, err)
	require.Equal(t, len(digests), len(resp.Responses))
	expectedCount := map[string]int{}
	for _, digest := range digests {
		if _, ok := expectedCount[digest.Hash]; ok {
			expectedCount[digest.Hash] = expectedCount[digest.Hash] + 1
		} else {
			expectedCount[digest.Hash] = 1
		}
	}
	actualCount := map[string]int{}
	for _, response := range resp.Responses {
		hash := response.Digest.Hash
		if _, ok := actualCount[hash]; ok {
			actualCount[hash] = actualCount[hash] + 1
		} else {
			actualCount[hash] = 1
		}
		if _, ok := blobs[hash]; ok {
			require.Equal(t, int32(codes.OK), response.Status.Code)
			require.Equal(t, blobs[hash], string(response.Data))
		} else {
			require.Equal(t, int32(codes.NotFound), response.Status.Code)
		}
	}
	require.Equal(t, expectedCount, actualCount)
}

func update(ctx context.Context, client repb.ContentAddressableStorageClient, blobs map[*repb.Digest]string, t testing.TB) {
	resp, err := client.BatchUpdateBlobs(ctx, updateBlobsRequest(blobs))
	require.NoError(t, err)
	require.Equal(t, len(blobs), len(resp.Responses))
	for i := 0; i < len(blobs); i++ {
		require.Equal(t, int32(codes.OK), resp.Responses[i].Status.Code)
	}
}

func expectAtimeUpdate(t *testing.T, clock clockwork.FakeClock, requestCount *atomic.Int32) {
	requestCount.Store(0)
	clock.Advance(atimeUpdatePeriod + time.Second)
	wait := time.Millisecond
	for i := 0; i < 7; i++ {
		time.Sleep(wait)
		wait = wait * 2
		if requestCount.Load() == 1 {
			requestCount.Store(0)
			return
		}
	}
	t.Fatal("Timed out waiting for remote atime update")
}

func expectNoAtimeUpdate(t *testing.T, clock clockwork.FakeClock, requestCount *atomic.Int32) {
	requestCount.Store(0)
	for i := 0; i < 10; i++ {
		clock.Advance(atimeUpdatePeriod + time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, int32(0), requestCount.Load())
}

func testContext() context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs(authutil.ClientIdentityHeaderName, "fakeheader"))
}

func TestFindMissingBlobs(t *testing.T) {
	ctx := testContext()
	conn, requestCount, _ := runRemoteCASS(ctx, testenv.GetTestEnv(t), t)
	proxyEnv := testenv.GetTestEnv(t)
	clock := clockwork.NewFakeClock()
	proxyEnv.SetClock(clock)
	proxyEnv.SetContentAddressableStorageClient(repb.NewContentAddressableStorageClient(conn))
	flags.Set(t, "cache_proxy.remote_atime_update_interval", atimeUpdatePeriod)
	require.NoError(t, atime_updater.Register(proxyEnv))
	proxyConn := runCASProxy(ctx, conn, proxyEnv, t)
	proxy := repb.NewContentAddressableStorageClient(proxyConn)

	fooDigestProto := digestProto(fooDigest, 3)
	barDigestProto := digestProto(barDigest, 3)

	for i := 1; i < 10; i++ {
		findMissing(ctx, proxy, []*repb.Digest{fooDigestProto}, []*repb.Digest{fooDigestProto}, t)
		require.Equal(t, int32(i), requestCount.Load())
	}
	expectNoAtimeUpdate(t, clock, requestCount)

	update(ctx, proxy, map[*repb.Digest]string{barDigestProto: "bar"}, t)

	requestCount.Store(0)
	for i := 1; i < 10; i++ {
		findMissing(ctx, proxy, []*repb.Digest{barDigestProto}, []*repb.Digest{}, t)
		require.Equal(t, int32(i), requestCount.Load())
	}
	expectNoAtimeUpdate(t, clock, requestCount)

	requestCount.Store(0)
	for i := 1; i < 10; i++ {
		findMissing(ctx, proxy, []*repb.Digest{fooDigestProto, barDigestProto}, []*repb.Digest{fooDigestProto}, t)
		require.Equal(t, int32(i), requestCount.Load())
	}
	expectNoAtimeUpdate(t, clock, requestCount)
}

func TestFindMissingBlobs_SkipRemote(t *testing.T) {
	ctx := testContext()
	remoteConn, _, _ := runRemoteCASS(ctx, testenv.GetTestEnv(t), t)
	proxyEnv := testenv.GetTestEnv(t)
	proxyEnv.SetContentAddressableStorageClient(repb.NewContentAddressableStorageClient(remoteConn))
	require.NoError(t, atime_updater.Register(proxyEnv))
	proxyConn := runCASProxy(ctx, remoteConn, proxyEnv, t)
	proxy := repb.NewContentAddressableStorageClient(proxyConn)

	digestA := digestProto(fooDigest, 3)
	digestB := digestProto(foofDigest, 4)
	digestC := digestProto(barDigest, 3)

	// Write digests A and B to the remote cache
	remote := proxyEnv.GetContentAddressableStorageClient()
	_, err := remote.BatchUpdateBlobs(ctx, updateBlobsRequest(map[*repb.Digest]string{digestA: "foo", digestB: "foof"}))
	require.NoError(t, err)

	// Write digests B and C to the local cache
	local := proxyEnv.GetLocalCASServer()
	_, err = local.BatchUpdateBlobs(ctx, updateBlobsRequest(map[*repb.Digest]string{digestB: "foof", digestC: "bar"}))
	require.NoError(t, err)

	// If hitting the remote cache, FindMissing(A, C) should return digest C
	req := findMissingBlobsRequest([]*repb.Digest{digestA, digestC})
	rsp, err := proxy.FindMissingBlobs(ctx, req)
	require.NoError(t, err)
	require.Equal(t, 1, len(rsp.MissingBlobDigests))
	require.Equal(t, digestC.GetHash(), rsp.MissingBlobDigests[0].Hash)

	// If only hitting the local proxy cache, FindMissing(A, C) should return digest A
	skipRemoteCtx := metadata.AppendToOutgoingContext(ctx, proxy_util.SkipRemoteKey, "true")
	rsp, err = proxy.FindMissingBlobs(skipRemoteCtx, req)
	require.NoError(t, err)
	require.Equal(t, 1, len(rsp.MissingBlobDigests))
	require.Equal(t, digestA.GetHash(), rsp.MissingBlobDigests[0].Hash)
}

func TestReadUpdateBlobs(t *testing.T) {
	ctx := testContext()
	conn, requestCount, _ := runRemoteCASS(ctx, testenv.GetTestEnv(t), t)
	casClient := repb.NewContentAddressableStorageClient(conn)
	proxyEnv := testenv.GetTestEnv(t)
	clock := clockwork.NewFakeClock()
	proxyEnv.SetClock(clock)
	proxyEnv.SetContentAddressableStorageClient(repb.NewContentAddressableStorageClient(conn))
	flags.Set(t, "cache_proxy.remote_atime_update_interval", atimeUpdatePeriod)
	require.NoError(t, atime_updater.Register(proxyEnv))
	proxyConn := runCASProxy(ctx, conn, proxyEnv, t)
	proxy := repb.NewContentAddressableStorageClient(proxyConn)

	fooDigestProto := digestProto(fooDigest, 3)
	foofDigestProto := digestProto(foofDigest, 4)
	barDigestProto := digestProto(barDigest, 3)
	barrDigestProto := digestProto(barrDigest, 4)
	barrrDigestProto := digestProto(barrrDigest, 5)
	bazDigestProto := digestProto(bazDigest, 3)
	quxDigestProto := digestProto(quxDigest, 3)

	read(ctx, proxy, []*repb.Digest{fooDigestProto, foofDigestProto, barDigestProto}, map[string]string{}, t)
	require.Equal(t, int32(1), requestCount.Load())
	expectNoAtimeUpdate(t, clock, requestCount)

	update(ctx, proxy, map[*repb.Digest]string{fooDigestProto: "foo"}, t)
	require.Equal(t, int32(1), requestCount.Load())
	expectNoAtimeUpdate(t, clock, requestCount)

	read(ctx, casClient, []*repb.Digest{fooDigestProto}, map[string]string{fooDigest: "foo"}, t)
	requestCount.Store(0)
	read(ctx, proxy, []*repb.Digest{fooDigestProto}, map[string]string{fooDigest: "foo"}, t)
	require.Equal(t, int32(0), requestCount.Load())
	expectAtimeUpdate(t, clock, requestCount)
	read(ctx, proxy, []*repb.Digest{fooDigestProto, fooDigestProto}, map[string]string{fooDigest: "foo"}, t)
	require.Equal(t, int32(0), requestCount.Load())
	expectAtimeUpdate(t, clock, requestCount)

	read(ctx, proxy, []*repb.Digest{barrDigestProto, barrrDigestProto, bazDigestProto}, map[string]string{}, t)
	require.Equal(t, int32(1), requestCount.Load())
	expectNoAtimeUpdate(t, clock, requestCount)
	update(ctx, casClient, map[*repb.Digest]string{bazDigestProto: "baz"}, t)
	require.Equal(t, int32(1), requestCount.Load())
	expectNoAtimeUpdate(t, clock, requestCount)

	read(ctx, proxy, []*repb.Digest{barrDigestProto, barrrDigestProto, bazDigestProto}, map[string]string{bazDigest: "baz"}, t)
	require.Equal(t, int32(1), requestCount.Load())
	expectNoAtimeUpdate(t, clock, requestCount)
	read(ctx, proxy, []*repb.Digest{fooDigestProto, bazDigestProto}, map[string]string{fooDigest: "foo", bazDigest: "baz"}, t)
	expectAtimeUpdate(t, clock, requestCount)

	update(ctx, casClient, map[*repb.Digest]string{quxDigestProto: "qux"}, t)
	read(ctx, proxy, []*repb.Digest{quxDigestProto, quxDigestProto}, map[string]string{quxDigest: "qux"}, t)
	expectNoAtimeUpdate(t, clock, requestCount)
}

func makeTree(ctx context.Context, client bspb.ByteStreamClient, t testing.TB) (*repb.Digest, []string) {
	child1 := uuid.New()
	digest1, files1 := cas.MakeTree(ctx, t, client, "", 2, 2)
	child2 := uuid.New()
	digest2, files2 := cas.MakeTree(ctx, t, client, "", 2, 2)

	// Upload a root directory containing both child directories.
	root := &repb.Directory{
		Directories: []*repb.DirectoryNode{
			&repb.DirectoryNode{
				Name:   child1,
				Digest: digest1,
			},
			&repb.DirectoryNode{
				Name:   child2,
				Digest: digest2,
			},
		},
	}
	rootDigest, err := cachetools.UploadProto(ctx, client, "", repb.DigestFunction_SHA256, root)
	require.NoError(t, err)
	children := append(files1, files2...)
	children = append(children, child1, child2)
	return rootDigest, children
}

func TestGetTree(t *testing.T) {
	testGetTree(t, true /* = withCaching */)
	testGetTree(t, false /* = withCaching */)
}

func testGetTree(t *testing.T, withCaching bool) {
	flags.Set(t, "cache_proxy.enable_get_tree_caching", withCaching)
	ctx := testContext()
	conn, unaryRequests, streamRequests := runRemoteCASS(ctx, testenv.GetTestEnv(t), t)
	casClient := repb.NewContentAddressableStorageClient(conn)
	bsClient := bspb.NewByteStreamClient(conn)
	proxyEnv := testenv.GetTestEnv(t)
	proxyEnv.SetAtimeUpdater(&testenv.NoOpAtimeUpdater{})
	proxyConn := runCASProxy(ctx, conn, proxyEnv, t)
	casProxy := repb.NewContentAddressableStorageClient(proxyConn)
	bsProxy := bspb.NewByteStreamClient(proxyConn)

	// Full tree written to the remote.
	rootDigest, files := makeTree(ctx, bsClient, t)
	treeFiles := cas.ReadTree(ctx, t, casClient, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	unaryRequests.Store(0)
	streamRequests.Store(0)
	treeFiles = cas.ReadTree(ctx, t, casProxy, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	if withCaching {
		// The tree has 4 levels, so expect 4 unary requests.
		require.Equal(t, int32(4), unaryRequests.Load())
		require.Equal(t, int32(0), streamRequests.Load())
	} else {
		require.Equal(t, int32(0), unaryRequests.Load())
		require.Equal(t, int32(1), streamRequests.Load())
	}
	unaryRequests.Store(0)
	streamRequests.Store(0)
	treeFiles = cas.ReadTree(ctx, t, casProxy, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	require.Equal(t, int32(0), unaryRequests.Load())
	if withCaching {
		require.Equal(t, int32(0), streamRequests.Load())
	} else {
		require.Equal(t, int32(1), streamRequests.Load())
	}

	// Full tree written to the proxy.
	rootDigest, files = makeTree(ctx, bsProxy, t)
	treeFiles = cas.ReadTree(ctx, t, casClient, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	unaryRequests.Store(0)
	streamRequests.Store(0)
	treeFiles = cas.ReadTree(ctx, t, casProxy, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	require.Equal(t, int32(0), unaryRequests.Load())
	if withCaching {
		require.Equal(t, int32(0), streamRequests.Load())
	} else {
		require.Equal(t, int32(1), streamRequests.Load())
	}

	// Write two subtrees to the proxy and a root node to the remote.
	firstTreeRoot, firstTreeFiles := makeTree(ctx, bsProxy, t)
	secondTreeRoot, secondTreeFiles := makeTree(ctx, bsProxy, t)
	root := &repb.Directory{
		Directories: []*repb.DirectoryNode{
			&repb.DirectoryNode{
				Name:   "first",
				Digest: firstTreeRoot,
			},
			&repb.DirectoryNode{
				Name:   "second",
				Digest: secondTreeRoot,
			},
		},
	}
	rootDigest, err := cachetools.UploadProto(ctx, bsClient, "", repb.DigestFunction_SHA256, root)
	files = []string{"first", "second"}
	files = append(files, firstTreeFiles...)
	files = append(files, secondTreeFiles...)
	require.NoError(t, err)
	treeFiles = cas.ReadTree(ctx, t, casClient, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	unaryRequests.Store(0)
	streamRequests.Store(0)
	treeFiles = cas.ReadTree(ctx, t, casProxy, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	if withCaching {
		// Only the root note should be read from the remote.
		require.Equal(t, int32(1), unaryRequests.Load())
		require.Equal(t, int32(0), streamRequests.Load())
	} else {
		require.Equal(t, int32(0), unaryRequests.Load())
		require.Equal(t, int32(1), streamRequests.Load())
	}

	// Write two subtrees to the remote and a root node to the proxy.
	firstTreeRoot, firstTreeFiles = makeTree(ctx, bsClient, t)
	secondTreeRoot, secondTreeFiles = makeTree(ctx, bsClient, t)
	root = &repb.Directory{
		Directories: []*repb.DirectoryNode{
			&repb.DirectoryNode{
				Name:   "first",
				Digest: firstTreeRoot,
			},
			&repb.DirectoryNode{
				Name:   "second",
				Digest: secondTreeRoot,
			},
		},
	}
	rootDigest, err = cachetools.UploadProto(ctx, bsProxy, "", repb.DigestFunction_SHA256, root)
	files = []string{"first", "second"}
	files = append(files, firstTreeFiles...)
	files = append(files, secondTreeFiles...)
	require.NoError(t, err)
	treeFiles = cas.ReadTree(ctx, t, casClient, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	unaryRequests.Store(0)
	streamRequests.Store(0)
	treeFiles = cas.ReadTree(ctx, t, casProxy, "", rootDigest)
	require.ElementsMatch(t, files, treeFiles)
	if withCaching {
		// The subtrees but not root should be read from the remote.
		require.Equal(t, int32(4), unaryRequests.Load())
		require.Equal(t, int32(0), streamRequests.Load())
	} else {
		require.Equal(t, int32(0), unaryRequests.Load())
		require.Equal(t, int32(1), streamRequests.Load())
	}
}

func BenchmarkFindMissingBlobs(b *testing.B) {
	ctx := testContext()
	conn, _, _ := runRemoteCASS(ctx, testenv.GetTestEnv(b), b)
	proxyEnv := testenv.GetTestEnv(b)
	clock := clockwork.NewFakeClock()
	proxyEnv.SetClock(clock)
	proxyEnv.SetContentAddressableStorageClient(repb.NewContentAddressableStorageClient(conn))
	require.NoError(b, atime_updater.Register(proxyEnv))
	proxyConn := runCASProxy(ctx, conn, proxyEnv, b)
	proxy := repb.NewContentAddressableStorageClient(proxyConn)

	fooDigestProto := digestProto(fooDigest, 3)
	foofDigestProto := digestProto(foofDigest, 4)
	barDigestProto := digestProto(barDigest, 3)
	barrDigestProto := digestProto(barrDigest, 4)
	barrrDigestProto := digestProto(barrrDigest, 5)
	bazDigestProto := digestProto(bazDigest, 3)
	quxDigestProto := digestProto(quxDigest, 3)
	update(ctx, proxy, map[*repb.Digest]string{
		fooDigestProto: "foo",
		barDigestProto: "bar",
		bazDigestProto: "baz",
		quxDigestProto: "qux",
	}, b)
	req := findMissingBlobsRequest([]*repb.Digest{
		fooDigestProto,
		foofDigestProto,
		barDigestProto,
		barrDigestProto,
		barrrDigestProto,
		bazDigestProto,
		quxDigestProto,
	})

	expected := []*repb.Digest{
		foofDigestProto,
		barrDigestProto,
		barrrDigestProto,
	}

	b.ReportAllocs()

	for b.Loop() {
		resp, err := proxy.FindMissingBlobs(ctx, req)
		require.NoError(b, err)
		require.ElementsMatch(b, expected, resp.GetMissingBlobDigests())
	}
}

func BenchmarkBatchReadBlobs(b *testing.B) {
	ctx := testContext()
	conn, _, _ := runRemoteCASS(ctx, testenv.GetTestEnv(b), b)
	proxyEnv := testenv.GetTestEnv(b)
	// The atime update runs background goroutines that can interfere with
	// calls to atime_updater.Enqueue(). Disable it for benchmarking.
	proxyEnv.SetAtimeUpdater(&testenv.NoOpAtimeUpdater{})
	clock := clockwork.NewFakeClock()
	proxyEnv.SetClock(clock)
	proxyEnv.SetContentAddressableStorageClient(repb.NewContentAddressableStorageClient(conn))
	require.NoError(b, atime_updater.Register(proxyEnv))
	proxyConn := runCASProxy(ctx, conn, proxyEnv, b)
	proxy := repb.NewContentAddressableStorageClient(proxyConn)

	fooDigestProto := digestProto(fooDigest, 3)
	foofDigestProto := digestProto(foofDigest, 4)
	barDigestProto := digestProto(barDigest, 3)
	barrDigestProto := digestProto(barrDigest, 4)
	barrrDigestProto := digestProto(barrrDigest, 5)
	bazDigestProto := digestProto(bazDigest, 3)
	quxDigestProto := digestProto(quxDigest, 3)
	blobs := map[*repb.Digest]string{
		fooDigestProto:   "foo",
		foofDigestProto:  "foof",
		barDigestProto:   "bar",
		barrDigestProto:  "barr",
		barrrDigestProto: "barrr",
		bazDigestProto:   "baz",
		quxDigestProto:   "qux",
	}

	update(ctx, proxy, blobs, b)
	req := readBlobsRequest(slices.Collect(maps.Keys(blobs)))

	b.ReportAllocs()

	for b.Loop() {
		resp, err := proxy.BatchReadBlobs(ctx, req)
		require.NoError(b, err)
		require.Equal(b, len(blobs), len(resp.GetResponses()))
	}
}

func BenchmarkBatchUpdateBlobs(b *testing.B) {
	ctx := testContext()
	conn, _, _ := runRemoteCASS(ctx, testenv.GetTestEnv(b), b)
	proxyEnv := testenv.GetTestEnv(b)
	clock := clockwork.NewFakeClock()
	proxyEnv.SetClock(clock)
	proxyEnv.SetContentAddressableStorageClient(repb.NewContentAddressableStorageClient(conn))
	require.NoError(b, atime_updater.Register(proxyEnv))
	proxyConn := runCASProxy(ctx, conn, proxyEnv, b)
	proxy := repb.NewContentAddressableStorageClient(proxyConn)

	fooDigestProto := digestProto(fooDigest, 3)
	foofDigestProto := digestProto(foofDigest, 4)
	barDigestProto := digestProto(barDigest, 3)
	barrDigestProto := digestProto(barrDigest, 4)
	barrrDigestProto := digestProto(barrrDigest, 5)
	bazDigestProto := digestProto(bazDigest, 3)
	quxDigestProto := digestProto(quxDigest, 3)
	blobs := map[*repb.Digest]string{
		fooDigestProto:   "foo",
		foofDigestProto:  "foof",
		barDigestProto:   "bar",
		barrDigestProto:  "barr",
		barrrDigestProto: "barrr",
		bazDigestProto:   "baz",
		quxDigestProto:   "qux",
	}

	i := 0
	req := updateBlobsRequest(blobs)

	b.ReportAllocs()

	for b.Loop() {
		req.InstanceName = fmt.Sprintf("%d", i)
		resp, err := proxy.BatchUpdateBlobs(ctx, req)
		require.NoError(b, err)
		require.Equal(b, len(blobs), len(resp.Responses))
		for i := 0; i < len(blobs); i++ {
			require.Equal(b, int32(codes.OK), resp.Responses[i].Status.Code)
		}
		i++
	}
}

func BenchmarkGetTree(b *testing.B) {
	flags.Set(b, "cache_proxy.enable_get_tree_caching", true)

	ctx := testContext()
	conn, unaryRequests, streamRequests := runRemoteCASS(ctx, testenv.GetTestEnv(b), b)
	proxyEnv := testenv.GetTestEnv(b)
	// The atime update runs background goroutines that can interfere with
	// calls to atime_updater.Enqueue(). Disable it for benchmarking.
	proxyEnv.SetAtimeUpdater(&testenv.NoOpAtimeUpdater{})
	proxyConn := runCASProxy(ctx, conn, proxyEnv, b)
	casProxy := repb.NewContentAddressableStorageClient(proxyConn)
	bsProxy := bspb.NewByteStreamClient(proxyConn)

	// Generate a static tree to reduce benchmark variability.
	fileDigests := map[string]*repb.Digest{}
	data := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, datum := range data {
		d, err := cachetools.UploadBlob(ctx, bsProxy, "", repb.DigestFunction_SHA256, bytes.NewReader([]byte(strings.Repeat(datum, 100))))
		require.NoError(b, err)
		fileDigests[datum] = d
	}

	subdirAB := &repb.Directory{
		Files: []*repb.FileNode{
			&repb.FileNode{Name: "a", Digest: fileDigests["a"]},
			&repb.FileNode{Name: "b", Digest: fileDigests["b"]},
		},
	}
	subdirCD := &repb.Directory{
		Files: []*repb.FileNode{
			&repb.FileNode{Name: "c", Digest: fileDigests["c"]},
			&repb.FileNode{Name: "d", Digest: fileDigests["d"]},
		},
	}
	subdirEF := &repb.Directory{
		Files: []*repb.FileNode{
			&repb.FileNode{Name: "e", Digest: fileDigests["e"]},
			&repb.FileNode{Name: "f", Digest: fileDigests["f"]},
		},
	}
	subdirGH := &repb.Directory{
		Files: []*repb.FileNode{
			&repb.FileNode{Name: "g", Digest: fileDigests["g"]},
			&repb.FileNode{Name: "h", Digest: fileDigests["h"]},
		},
	}
	abDigest, err := cachetools.UploadProto(ctx, bsProxy, "", repb.DigestFunction_SHA256, subdirAB)
	require.NoError(b, err)
	cdDigest, err := cachetools.UploadProto(ctx, bsProxy, "", repb.DigestFunction_SHA256, subdirCD)
	require.NoError(b, err)
	efDigest, err := cachetools.UploadProto(ctx, bsProxy, "", repb.DigestFunction_SHA256, subdirEF)
	require.NoError(b, err)
	ghDigest, err := cachetools.UploadProto(ctx, bsProxy, "", repb.DigestFunction_SHA256, subdirGH)
	require.NoError(b, err)

	subdirABCD := &repb.Directory{
		Directories: []*repb.DirectoryNode{
			&repb.DirectoryNode{Name: "ab", Digest: abDigest},
			&repb.DirectoryNode{Name: "cd", Digest: cdDigest},
		},
	}
	subdirEFGH := &repb.Directory{
		Directories: []*repb.DirectoryNode{
			&repb.DirectoryNode{Name: "ef", Digest: efDigest},
			&repb.DirectoryNode{Name: "gh", Digest: ghDigest},
		},
	}
	abcdDigest, err := cachetools.UploadProto(ctx, bsProxy, "", repb.DigestFunction_SHA256, subdirABCD)
	require.NoError(b, err)
	efghDigest, err := cachetools.UploadProto(ctx, bsProxy, "", repb.DigestFunction_SHA256, subdirEFGH)
	require.NoError(b, err)

	root := &repb.Directory{
		Directories: []*repb.DirectoryNode{
			&repb.DirectoryNode{Name: "abcd", Digest: abcdDigest},
			&repb.DirectoryNode{Name: "efgh", Digest: efghDigest},
		},
	}
	rootDigest, err := cachetools.UploadProto(ctx, bsProxy, "", repb.DigestFunction_SHA256, root)
	require.NoError(b, err)

	files := []string{"a", "b", "c", "d", "e", "f", "g", "h", "ab", "cd", "ef", "gh", "abcd", "efgh"}

	// Warm up tree cache
	treeFiles := cas.ReadTree(ctx, b, casProxy, "", rootDigest)
	require.ElementsMatch(b, files, treeFiles)
	unaryRequests.Store(0)
	streamRequests.Store(0)

	b.ReportAllocs()

	for b.Loop() {
		treeFiles := cas.ReadTree(ctx, b, casProxy, "", rootDigest)
		require.ElementsMatch(b, files, treeFiles)
		require.Equal(b, int32(0), unaryRequests.Load())
		require.Equal(b, int32(0), streamRequests.Load())
	}

}
