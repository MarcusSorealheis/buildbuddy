load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "options",
    srcs = ["options.go"],
    importpath = "github.com/buildbuddy-io/buildbuddy/cli/parser/options",
    visibility = ["//visibility:public"],
    deps = [
        "//cli/log",
        "//cli/parser/arguments",
        "//cli/parser/bazelrc",
        "//proto:bazel_flags_go_proto",
        "//server/util/lib/seq",
        "//server/util/lib/set",
    ],
)

package(default_visibility = ["//cli:__subpackages__"])
