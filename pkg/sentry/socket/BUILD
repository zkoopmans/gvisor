load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "socket",
    srcs = [
        "socket.go",
        "socket_state.go",
    ],
    visibility = ["//pkg/sentry:internal"],
    deps = [
        "//pkg/abi/linux",
        "//pkg/atomicbitops",
        "//pkg/context",
        "//pkg/hostarch",
        "//pkg/marshal",
        "//pkg/sentry/kernel",
        "//pkg/sentry/ktime",
        "//pkg/sentry/socket/unix/transport",
        "//pkg/sentry/vfs",
        "//pkg/syserr",
        "//pkg/tcpip",
        "//pkg/tcpip/header",
        "//pkg/usermem",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
