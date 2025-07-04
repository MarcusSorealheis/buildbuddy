package platform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buildbuddy-io/buildbuddy/server/testutil/testenv"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/testing/flags"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/testing/protocmp"

	fcpb "github.com/buildbuddy-io/buildbuddy/proto/firecracker"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	scpb "github.com/buildbuddy-io/buildbuddy/proto/scheduler"
	gstatus "google.golang.org/grpc/status"
)

var (
	bare                 = &ExecutorProperties{SupportedIsolationTypes: []ContainerType{BareContainerType}}
	docker               = &ExecutorProperties{SupportedIsolationTypes: []ContainerType{DockerContainerType}}
	podmanAndFirecracker = &ExecutorProperties{SupportedIsolationTypes: []ContainerType{PodmanContainerType, FirecrackerContainerType}}
)

func TestParse_ContainerImage_Success(t *testing.T) {
	flags.Set(t, "executor.container_registry_region", "us-test1")
	for _, testCase := range []struct {
		execProps         *ExecutorProperties
		imageProp         string
		containerImageKey string
		expected          string
	}{
		{bare, "", "container-image", ""},
		{bare, "", "Container-Image", ""},
		{bare, "none", "container-image", ""},
		{bare, "none", "Container-Image", ""},
		{bare, "None", "container-image", ""},
		{bare, "None", "Container-Image", ""},
		{docker, "", "container-image", *defaultImage},
		{docker, "", "Container-Image", *defaultImage},
		{docker, "none", "container-image", *defaultImage},
		{docker, "none", "Container-Image", *defaultImage},
		{docker, "None", "container-image", *defaultImage},
		{docker, "None", "Container-Image", *defaultImage},
		{docker, "docker://alpine", "container-image", "alpine"},
		{docker, "docker://alpine", "Container-Image", "alpine"},
		{docker, "docker://caseSensitiveUrl", "container-image", "caseSensitiveUrl"},
		{docker, "docker://{{region}}.gcr.io/{{region}}-ubuntu:latest", "container-image", "us-test1.gcr.io/us-test1-ubuntu:latest"},
		{docker, "docker://{{region}}.gcr.io/{{region}}-ubuntu:latest", "Container-image", "us-test1.gcr.io/us-test1-ubuntu:latest"},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: testCase.containerImageKey, Value: testCase.imageProp},
		}}

		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		env := testenv.GetTestEnv(t)
		env.SetXcodeLocator(&xcodeLocator{})
		err = ApplyOverrides(env, testCase.execProps, platformProps, &repb.Command{})
		require.NoError(t, err)
		assert.Equal(t, testCase.expected, platformProps.ContainerImage, testCase)
	}
}

func TestParse_ContainerImage_Error(t *testing.T) {
	for _, testCase := range []struct {
		execProps *ExecutorProperties
		imageProp string
	}{
		{bare, "docker://alpine"},
		{bare, "invalid"},
		{bare, "invalid://alpine"},
		{docker, "invalid"},
		{docker, "invalid://alpine"},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: "container-image", Value: testCase.imageProp},
		}}

		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		env := testenv.GetTestEnv(t)
		env.SetXcodeLocator(&xcodeLocator{})
		err = ApplyOverrides(env, testCase.execProps, platformProps, &repb.Command{})
		assert.Error(t, err)
	}
}

func TestParse_OS(t *testing.T) {
	for _, testCase := range []struct {
		rawValue      string
		expectedValue string
	}{
		{"", "linux"},
		{"linux", "linux"},
		{"darwin", "darwin"},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: "OSFamily", Value: testCase.rawValue},
		}}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		assert.Equal(t, testCase.expectedValue, platformProps.OS)
	}

	// Empty case
	plat := &repb.Platform{Properties: []*repb.Platform_Property{}}
	platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
	require.NoError(t, err)
	assert.Equal(t, "linux", platformProps.OS)
}

func TestParse_Arch(t *testing.T) {
	for _, testCase := range []struct {
		rawValue      string
		expectedValue string
	}{
		{"", "amd64"},
		{"amd64", "amd64"},
		{"arm64", "arm64"},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: "Arch", Value: testCase.rawValue},
		}}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		assert.Equal(t, testCase.expectedValue, platformProps.Arch)
	}

	// Empty case
	plat := &repb.Platform{Properties: []*repb.Platform_Property{}}
	platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
	require.NoError(t, err)
	assert.Equal(t, "amd64", platformProps.Arch)
}

func TestParse_Pool(t *testing.T) {
	for _, testCase := range []struct {
		rawValue      string
		expectedValue string
	}{
		{"", ""},
		{"default", ""},
		{"my-pool", "my-pool"},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: "Pool", Value: testCase.rawValue},
		}}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		assert.Equal(t, testCase.expectedValue, platformProps.Pool)
	}

	// Empty case
	plat := &repb.Platform{Properties: []*repb.Platform_Property{}}
	platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
	require.NoError(t, err)
	assert.Equal(t, "", platformProps.Pool)
}

func TestParse_EstimatedBCU(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		rawValue      string
		expectedValue float64
	}{
		{"EstimatedComputeUnits", "", 0},
		{"EstimatedComputeUnits", "NOT_A_VALID_NUMBER", 0},
		{"EstimatedComputeUnits", "0", 0},
		{"EstimatedComputeUnits", "1", 1},
		{"EstimatedComputeUnits", " 1 ", 1},
		{"estimatedcomputeunits", "1", 1},
		{"EstimatedComputeUnits", "0.5", 0.5},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: testCase.name, Value: testCase.rawValue},
		}}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		assert.Equal(t, testCase.expectedValue, platformProps.EstimatedComputeUnits)
	}
}

func TestParse_EstimatedFreeDisk(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		rawValue      string
		expectedValue int64
	}{
		{"EstimatedFreeDiskBytes", "", 0},
		{"EstimatedFreeDiskBytes", "NOT_AN_INT", 0},
		{"EstimatedFreeDiskBytes", "0", 0},
		{"EstimatedFreeDiskBytes", "1", 1},
		{"EstimatedFreeDiskBytes", " 1 ", 1},
		{"estimatedfreediskbytes", "1", 1},
		{"EstimatedFreeDiskBytes", "1000B", 1000},
		{"EstimatedFreeDiskBytes", "1e3", 1000},
		{"EstimatedFreeDiskBytes", "1M", 1024 * 1024},
		{"EstimatedFreeDiskBytes", "1GB", 1024 * 1024 * 1024},
		{"EstimatedFreeDiskBytes", "2.0GB", 2 * 1024 * 1024 * 1024},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: testCase.name, Value: testCase.rawValue},
		}}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		assert.Equal(t, testCase.expectedValue, platformProps.EstimatedFreeDiskBytes)
	}
}

func TestParse_EstimatedCPU(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		rawValue      string
		expectedValue int64
	}{
		{"EstimatedCPU", "", 0},
		{"EstimatedCPU", "0.5", 500},
		{"EstimatedCPU", "1", 1000},
		{"EstimatedCPU", "+0.1e+1", 1000},
		{"EstimatedCPU", "4000m", 4000},
		{"EstimatedCPU", "4e3m", 4000},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: testCase.name, Value: testCase.rawValue},
		}}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		assert.Equal(t, testCase.expectedValue, platformProps.EstimatedMilliCPU)
	}
}

func TestParse_EstimatedMemory(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		rawValue      string
		expectedValue int64
	}{
		{"EstimatedMemory", "", 0},
		{"EstimatedMemory", "1000B", 1000},
		{"EstimatedMemory", "1e3", 1000},
		{"EstimatedMemory", "1M", 1024 * 1024},
		{"EstimatedMemory", "1GB", 1024 * 1024 * 1024},
		{"EstimatedMemory", "2.0GB", 2 * 1024 * 1024 * 1024},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: testCase.name, Value: testCase.rawValue},
		}}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		assert.Equal(t, testCase.expectedValue, platformProps.EstimatedMemoryBytes)
	}
}

func TestParse_Duration(t *testing.T) {
	const durationProperty = "runner-recycling-max-wait"

	// Invalid values:
	for _, rawValue := range []string{
		"100",
		"blah",
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: durationProperty, Value: rawValue},
		}}
		_, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.Error(t, err, "parse %q", rawValue)
	}

	// Valid values:
	for _, testCase := range []struct {
		rawValue      string
		expectedValue time.Duration
	}{
		{"", 0},
		{"10ms", 10 * time.Millisecond},
		{"-20ms", -20 * time.Millisecond},
		{"2s", 2 * time.Second},
		{"4m", 4 * time.Minute},
		{"-7m", -7 * time.Minute},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: durationProperty, Value: testCase.rawValue},
		}}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		assert.Equal(t, testCase.expectedValue, platformProps.RunnerRecyclingMaxWait)
	}
}

func TestParse_CustomResources_Valid(t *testing.T) {
	props := []*repb.Platform_Property{
		{Name: "resources:foo", Value: "3.14"},
	}
	task := &repb.ExecutionTask{Command: &repb.Command{Platform: &repb.Platform{Properties: props}}}
	p, err := ParseProperties(task)
	require.NoError(t, err)
	require.Empty(t, cmp.Diff([]*scpb.CustomResource{{
		Name: "foo", Value: 3.14,
	}}, p.CustomResources, protocmp.Transform()))
}

func TestParse_CustomResources_Invalid(t *testing.T) {
	props := []*repb.Platform_Property{
		{Name: "resources:foo", Value: "blah"},
	}
	task := &repb.ExecutionTask{Command: &repb.Command{Platform: &repb.Platform{Properties: props}}}
	_, err := ParseProperties(task)
	require.True(t, status.IsInvalidArgumentError(err), "expected InvalidArgument, got %s", gstatus.Code(err))
}

func TestParse_OverrideSnapshotKey(t *testing.T) {
	key := &fcpb.SnapshotKey{
		SnapshotId:        "snapshot-id",
		InstanceName:      "instance-name",
		PlatformHash:      "platform-hash",
		ConfigurationHash: "config-hash",
		RunnerId:          "runner-id",
		Ref:               "ref",
		VersionId:         "version-id",
	}
	keyBytes, err := json.Marshal(key)
	require.NoError(t, err)
	props := []*repb.Platform_Property{
		{Name: SnapshotKeyOverridePropertyName, Value: string(keyBytes)},
	}
	task := &repb.ExecutionTask{Command: &repb.Command{Platform: &repb.Platform{Properties: props}}}
	p, err := ParseProperties(task)
	require.NoError(t, err)
	require.Empty(t, cmp.Diff(key, p.OverrideSnapshotKey, protocmp.Transform()))
}

func TestParse_ApplyOverrides(t *testing.T) {
	for _, testCase := range []struct {
		platformProps       []*repb.Platform_Property
		startingEnvVars     []*repb.Command_EnvironmentVariable
		expectedEnvVars     []*repb.Command_EnvironmentVariable
		errorExpected       bool
		defaultXcodeVersion string
	}{
		// Default darwin platform
		{[]*repb.Platform_Property{
			{Name: "osfamily", Value: "darwin"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "SDKROOT", Value: "/Applications/Xcode_12.4.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk"},
			{Name: "DEVELOPER_DIR", Value: "/Applications/Xcode_12.4.app/Contents/Developer"},
		},
			false,
			"12.2",
		},
		// Case insensitive darwin platform
		{[]*repb.Platform_Property{
			{Name: "OSFAMILY", Value: "dArWiN"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "SDKROOT", Value: "/Applications/Xcode_12.4.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk"},
			{Name: "DEVELOPER_DIR", Value: "/Applications/Xcode_12.4.app/Contents/Developer"},
		},
			false,
			"12.2",
		},
		// Darwin with no overrides
		{[]*repb.Platform_Property{
			{Name: "OSFamily", Value: "Darwin"},
		}, []*repb.Command_EnvironmentVariable{}, []*repb.Command_EnvironmentVariable{
			{Name: "SDKROOT", Value: "/Applications/Xcode_12.2.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk"},
			{Name: "DEVELOPER_DIR", Value: "/Applications/Xcode_12.2.app/Contents/Developer"},
		},
			false,
			"12.2",
		},
		// Darwin with invalid sdk platform
		{[]*repb.Platform_Property{
			{Name: "OSFamily", Value: "Darwin"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_VERSION_OVERRIDE", Value: "14.1"},
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{},
			true,
			"12.2",
		},
		// Darwin with valid sdk platform
		{[]*repb.Platform_Property{
			{Name: "OSFamily", Value: "Darwin"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_VERSION_OVERRIDE", Value: "14.3"},
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "SDKROOT", Value: "/Applications/Xcode_12.4.app/Contents/Developer/Platforms/iPhone.platform/Developer/SDKs/iPhone14.3.sdk"},
			{Name: "DEVELOPER_DIR", Value: "/Applications/Xcode_12.4.app/Contents/Developer"},
		},
			false,
			"12.2",
		},
		// Darwin with xcode override but no sdk version
		{[]*repb.Platform_Property{
			{Name: "OSFamily", Value: "Darwin"},
			{Name: "enablexcodeoverride", Value: "true"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "SDKROOT", Value: "/Applications/Xcode_12.4.app/Contents/Developer/Platforms/iPhone.platform/Developer/SDKs/iPhone.sdk"},
			{Name: "DEVELOPER_DIR", Value: "/Applications/Xcode_12.4.app/Contents/Developer"},
		},
			false,
			"12.2",
		},
		// Darwin with xcode override but invalid sdk version
		{[]*repb.Platform_Property{
			{Name: "OSFamily", Value: "Darwin"},
			{Name: "enablexcodeoverride", Value: "true"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_VERSION_OVERRIDE", Value: "14.2"},
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{},
			true,
			"12.2",
		},
		// Case insensitive darwin with xcode override
		{[]*repb.Platform_Property{
			{Name: "OSFAMILY", Value: "dArWiN"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_VERSION_OVERRIDE", Value: "14.3"},
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "SDKROOT", Value: "/Applications/Xcode_12.4.app/Contents/Developer/Platforms/iPhone.platform/Developer/SDKs/iPhone14.3.sdk"},
			{Name: "DEVELOPER_DIR", Value: "/Applications/Xcode_12.4.app/Contents/Developer"},
		},
			false,
			"12.2",
		},
		// Case insensitive darwin with no default xcode version
		{[]*repb.Platform_Property{
			{Name: "OSFAMILY", Value: "dArWiN"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.5.123"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "SDKROOT", Value: "/Applications/Xcode.app/Contents/Developer/Platforms/iPhone.platform/Developer/SDKs/iPhone.sdk"},
			{Name: "DEVELOPER_DIR", Value: "/Applications/Xcode.app/Contents/Developer"},
		},
			false,
			"",
		},
		// Case insensitive darwin with no default xcode version and existing sdk
		{[]*repb.Platform_Property{
			{Name: "OSFAMILY", Value: "dArWiN"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_VERSION_OVERRIDE", Value: "14.3"},
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "SDKROOT", Value: "/Applications/Xcode_12.4.app/Contents/Developer/Platforms/iPhone.platform/Developer/SDKs/iPhone14.3.sdk"},
			{Name: "DEVELOPER_DIR", Value: "/Applications/Xcode_12.4.app/Contents/Developer"},
		},
			false,
			"",
		},
		// Default linux
		{[]*repb.Platform_Property{
			{Name: "osfamily", Value: "linux"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_VERSION_OVERRIDE", Value: "11.1"},
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{},
			false,
			"12.2",
		},
		// Default case-insensitive linux
		{[]*repb.Platform_Property{
			{Name: "oSfAmIlY", Value: "LINUX"},
		}, []*repb.Command_EnvironmentVariable{
			{Name: "APPLE_SDK_VERSION_OVERRIDE", Value: "11.1"},
			{Name: "APPLE_SDK_PLATFORM", Value: "iPhone"},
			{Name: "XCODE_VERSION_OVERRIDE", Value: "12.4.123"},
		}, []*repb.Command_EnvironmentVariable{},
			false,
			"12.2",
		},
	} {
		plat := &repb.Platform{Properties: testCase.platformProps}
		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		execProps := bare
		execProps.DefaultXcodeVersion = testCase.defaultXcodeVersion
		command := &repb.Command{EnvironmentVariables: testCase.startingEnvVars}
		env := testenv.GetTestEnv(t)
		env.SetXcodeLocator(&xcodeLocator{
			sdks12_2: map[string]string{
				"iPhone":     "Platforms/iPhone.platform/Developer/SDKs/iPhone.sdk",
				"iPhone14.2": "Platforms/iPhone.platform/Developer/SDKs/iPhone14.2.sdk",
				"MacOSX":     "Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk",
				"MacOSX11.0": "Platforms/MacOSX.platform/Developer/SDKs/MacOSX11.0.sdk",
			},
			sdks12_4: map[string]string{
				"iPhone":     "Platforms/iPhone.platform/Developer/SDKs/iPhone.sdk",
				"iPhone14.3": "Platforms/iPhone.platform/Developer/SDKs/iPhone14.3.sdk",
				"MacOSX":     "Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk",
				"MacOSX11.1": "Platforms/MacOSX.platform/Developer/SDKs/MacOSX11.1.sdk",
			},
			sdksDefault: map[string]string{
				"iPhone":     "Platforms/iPhone.platform/Developer/SDKs/iPhone.sdk",
				"iPhone14.4": "Platforms/iPhone.platform/Developer/SDKs/iPhone14.4.sdk",
				"MacOSX":     "Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk",
				"MacOSX11.3": "Platforms/MacOSX.platform/Developer/SDKs/MacOSX11.3.sdk",
			},
		})
		err = ApplyOverrides(env, execProps, platformProps, command)
		if testCase.errorExpected {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		assert.ElementsMatch(t, command.EnvironmentVariables, append(testCase.startingEnvVars, testCase.expectedEnvVars...))
	}
}

func TestEnvAndArgOverrides(t *testing.T) {
	plat := &repb.Platform{Properties: []*repb.Platform_Property{
		{Name: "env-overrides", Value: "A=1,B=2,A=3"},
		{Name: "env-overrides-base64", Value: base64.StdEncoding.EncodeToString([]byte(`C={"some":1,"value":2}`))},
		{Name: "extra-args", Value: "--foo,--bar=baz"},
	}}
	platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
	require.NoError(t, err)
	execProps := bare
	command := &repb.Command{
		Arguments: []string{"./some_cmd"},
		EnvironmentVariables: []*repb.Command_EnvironmentVariable{
			{Name: "A", Value: "0"},
		},
	}
	env := testenv.GetTestEnv(t)
	err = ApplyOverrides(env, execProps, platformProps, command)
	require.NoError(t, err)

	expectedCmd := &repb.Command{
		Arguments: []string{"./some_cmd", "--foo", "--bar=baz"},
		EnvironmentVariables: []*repb.Command_EnvironmentVariable{
			// Should just tack on env vars as-is. Runner implementations will ensure
			// that if there are multiple with the same name, the last one wins.
			{Name: "A", Value: "0"},
			{Name: "A", Value: "1"},
			{Name: "B", Value: "2"},
			{Name: "A", Value: "3"},
			{Name: "C", Value: `{"some":1,"value":2}`},
		},
	}

	expectedCmdText, err := prototext.Marshal(expectedCmd)
	require.NoError(t, err)
	commandText, err := prototext.Marshal(command)
	require.NoError(t, err)
	require.Equal(t, expectedCmdText, commandText)
}

func TestEnvOverridesError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"not_base64_encode", "D=123"},
		{"mixed_base64", base64.StdEncoding.EncodeToString([]byte(`C={"some":1,"value":2}`)) + "D=123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plat := &repb.Platform{Properties: []*repb.Platform_Property{
				{Name: "env-overrides-base64", Value: tc.value},
			}}
			props, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
			require.Error(t, err)
			require.True(t, status.IsInvalidArgumentError(err), "expected InvalidArgument, got %s", gstatus.Code(err))
			require.Nil(t, props)
		})
	}
}

func TestForceNetworkIsolationType(t *testing.T) {
	for _, testCase := range []struct {
		dockerNetworkValue         string
		workloadIsolationType      string
		forcedNetworkIsolationType string
		expectedIsolationType      string
	}{
		// No override set -- behavior unchanged.
		{"", "podman", "", "podman"},
		{"host", "podman", "", "podman"},
		{"none", "podman", "", "podman"},

		// Override set: everything except "none" should
		// trigger an override.
		{"", "podman", "firecracker", "firecracker"},
		{"host", "podman", "firecracker", "firecracker"},
		{"none", "podman", "firecracker", "podman"},
	} {
		plat := &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: "container-image", Value: "docker://alpine"},
			{Name: "dockerNetwork", Value: testCase.dockerNetworkValue},
			{Name: "workload-isolation-type", Value: testCase.workloadIsolationType},
		}}

		flags.Set(t, "executor.forced_network_isolation_type", testCase.forcedNetworkIsolationType)

		platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
		require.NoError(t, err)
		env := testenv.GetTestEnv(t)
		env.SetXcodeLocator(&xcodeLocator{})
		err = ApplyOverrides(env, podmanAndFirecracker, platformProps, &repb.Command{})
		assert.NoError(t, err)
		assert.Equal(t, testCase.expectedIsolationType, platformProps.WorkloadIsolationType, testCase)
	}
}

func TestExtraEnvVars(t *testing.T) {
	for _, tc := range []struct {
		name            string
		extraEnvVars    []string
		expectedEnvVars []*repb.Command_EnvironmentVariable
	}{
		{
			name:         "set env var to value",
			extraEnvVars: []string{"FOO=bar"},
			expectedEnvVars: []*repb.Command_EnvironmentVariable{
				{Name: "FOO", Value: "bar"},
			},
		},
		{
			name:         "set env var to value containing =",
			extraEnvVars: []string{"FOO=bar=baz"},
			expectedEnvVars: []*repb.Command_EnvironmentVariable{
				{Name: "FOO", Value: "bar=baz"},
			},
		},
		{
			name:         "inherit process env var",
			extraEnvVars: []string{"FOO"},
			expectedEnvVars: []*repb.Command_EnvironmentVariable{
				{Name: "FOO", Value: "process-foo-value"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FOO", "process-foo-value")
			flags.Set(t, "executor.extra_env_vars", tc.extraEnvVars)

			env := testenv.GetTestEnv(t)
			cmd := &repb.Command{}
			platformProps := &Properties{}
			ApplyOverrides(env, podmanAndFirecracker, platformProps, cmd)
			require.Empty(t, cmp.Diff(
				tc.expectedEnvVars,
				cmd.EnvironmentVariables,
				protocmp.Transform(),
			))
		})
	}
}

func TestPersistentVolumes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		prop          string
		expected      []PersistentVolume
		expectedError error
	}{
		{
			name: "one volume",
			prop: "cache:/tmp/.cache",
			expected: []PersistentVolume{
				{name: "cache", containerPath: "/tmp/.cache"},
			},
			expectedError: nil,
		},
		{
			name: "multiple volumes",
			prop: "tmp_cache:/tmp/.cache,user_cache:/root/.cache",
			expected: []PersistentVolume{
				{name: "tmp_cache", containerPath: "/tmp/.cache"},
				{name: "user_cache", containerPath: "/root/.cache"},
			},
			expectedError: nil,
		},
		{
			name: "invalid volume name",
			prop: "..:/tmp/.cache",
			expected: []PersistentVolume{
				{name: "cache", containerPath: "/tmp/.cache"},
			},
			expectedError: status.InvalidArgumentError(`invalid persistent volume "..:/tmp/.cache": name can only contain alphanumeric characters, hyphens, and underscores`),
		},
		{
			name:          "empty volume name",
			prop:          ":/tmp/.cache",
			expected:      nil,
			expectedError: status.InvalidArgumentError(`invalid persistent volume ":/tmp/.cache": expected "<name>:<container_path>"`),
		},
		{
			name:          "empty mount path",
			prop:          "cache:",
			expected:      nil,
			expectedError: status.InvalidArgumentError(`invalid persistent volume "cache:": expected "<name>:<container_path>"`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plat := &repb.Platform{Properties: []*repb.Platform_Property{
				{Name: "persistent-volumes", Value: tc.prop},
			}}
			platformProps, err := ParseProperties(&repb.ExecutionTask{Command: &repb.Command{Platform: plat}})
			if tc.expectedError == nil {
				require.Equal(t, tc.expected, platformProps.PersistentVolumes)
				require.NoError(t, err)
			} else {
				require.Nil(t, platformProps)
				require.Equal(t, tc.expectedError, err)
			}
		})
	}
}

type xcodeLocator struct {
	sdks12_2    map[string]string
	sdks12_4    map[string]string
	sdksDefault map[string]string
}

func (x *xcodeLocator) PathsForVersionAndSDK(xcodeVersion string, sdk string) (string, string, error) {
	var developerDir string
	var sdkPath string
	if strings.HasPrefix(xcodeVersion, "12.2") {
		developerDir = "/Applications/Xcode_12.2.app/Contents/Developer"
		sdkPath = x.sdks12_2[sdk]
	} else if strings.HasPrefix(xcodeVersion, "12.4") {
		developerDir = "/Applications/Xcode_12.4.app/Contents/Developer"
		sdkPath = x.sdks12_4[sdk]
	} else {
		developerDir = "/Applications/Xcode.app/Contents/Developer"
		sdkPath = x.sdksDefault[sdk]
	}
	if sdkPath == "" {
		return "", "", fmt.Errorf("Invalid SDK '%s' for Xcode '%s'", sdk, xcodeVersion)
	}
	sdkRoot := fmt.Sprintf("%s/%s", developerDir, sdkPath)
	return developerDir, sdkRoot, nil
}
