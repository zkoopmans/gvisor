load("//tools:defs.bzl", "go_binary", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "funnel",
    srcs = [
        "assign.go",
        "floor.go",
        "main.go",
    ],
    nogo = False,
    deps = [
        "@com_github_google_go_github_v84//github:go_default_library",
        "@in_gopkg_yaml_v3//:go_default_library",
    ],
)

go_test(
    name = "funnel_test",
    size = "small",
    srcs = [
        "assign.go",
        "assign_test.go",
        "floor.go",
        "floor_test.go",
    ],
    nogo = False,
    deps = ["@in_gopkg_yaml_v3//:go_default_library"],
)
