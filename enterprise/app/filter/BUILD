load("//rules/typescript:index.bzl", "ts_library")

package(default_visibility = ["//enterprise:__subpackages__"])

ts_library(
    name = "filter",
    srcs = ["filter.tsx"],
    deps = [
        "//:node_modules/@types/react",
        "//:node_modules/lucide-react",
        "//:node_modules/react",
        "//:node_modules/tslib",
        "//app/capabilities",
        "//app/components/button",
        "//app/components/checkbox",
        "//app/components/input",
        "//app/components/popup",
        "//app/components/radio",
        "//app/components/slider",
        "//app/format",
        "//app/router",
        "//app/router:router_params",
        "//enterprise/app/filter:date_picker_button",
        "//enterprise/app/filter:filter_util",
        "//proto:invocation_status_ts_proto",
        "//proto:stat_filter_ts_proto",
    ],
)

filegroup(
    name = "css",
    srcs = [
        ":filter.css",
        ":react_date_range_base.css",
        ":react_date_range_default_theme.css",
    ],
)

ts_library(
    name = "date_picker_button",
    srcs = ["date_picker_button.tsx"],
    deps = [
        "//:node_modules/@types/react",
        "//:node_modules/@types/react-date-range",
        "//:node_modules/lucide-react",
        "//:node_modules/moment",
        "//:node_modules/react",
        "//:node_modules/react-date-range",
        "//:node_modules/tslib",
        "//app/components/button",
        "//app/components/popup",
        "//app/format",
        "//app/router",
        "//app/router:router_params",
        "//enterprise/app/filter:filter_util",
    ],
)

ts_library(
    name = "filter_util",
    srcs = ["filter_util.tsx"],
    deps = [
        "//:node_modules/@types/long",
        "//:node_modules/long",
        "//:node_modules/moment",
        "//:node_modules/tslib",
        "//app/capabilities",
        "//app/format",
        "//app/router:router_params",
        "//app/util:proto",
        "//proto:duration_ts_proto",
        "//proto:invocation_status_ts_proto",
        "//proto:stat_filter_ts_proto",
        "//proto:timestamp_ts_proto",
    ],
)
