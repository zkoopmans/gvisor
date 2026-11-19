load("//tools:defs.bzl", "go_binary", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "funnel",
    srcs = [
        "floor.go",
        "main.go",
    ],
    nogo = False,
    deps = ["@com_github_google_go_github_v84//github:go_default_library"],
)

go_test(
    name = "funnel_test",
    size = "small",
    srcs = [
        "floor.go",
        "floor_test.go",
    ],
    nogo = False,
)
