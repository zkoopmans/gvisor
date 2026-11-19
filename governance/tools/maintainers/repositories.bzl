# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Validate area directories before exporting the generator's YAML input."""

load("@bazel_lib//lib:repo_utils.bzl", "repo_utils")
load("@yq.bzl//yq/toolchain:platforms.bzl", "yq_platform_repo")
load("@yq.bzl//yq/toolchain:versions.bzl", "DEFAULT_YQ_VERSION")

def _segment_matches(pattern, name):
    """Reports whether name matches pattern, where `*` matches any run of
    characters within the segment."""
    parts = pattern.split("*")
    if not name.startswith(parts[0]) or not name.endswith(parts[-1]):
        return False
    if len(name) < len(parts[0]) + len(parts[-1]):
        return False
    pos = len(parts[0])
    for part in parts[1:-1]:
        index = name.find(part, pos)
        if index < 0:
            return False
        pos = index + len(part)
    return pos <= len(name) - len(parts[-1])

def _governance_areas_impl(ctx):
    # Remote generation cannot inspect undeclared checkout directories. Check
    # the listed paths here, anchored to this module even when it is a dependency.
    root = ctx.path(ctx.attr.root).dirname
    areas = ctx.path(ctx.attr.areas)
    ctx.watch(areas)
    result = ctx.execute([ctx.path(ctx.attr.yq), "-o=json", "[.areas[].paths[]]", areas])
    if result.return_code:
        fail("Cannot read governance area paths: " + result.stderr)
    for path in json.decode(result.stdout):
        relative = path.removeprefix("/")
        if "*" not in relative:
            directory = root.get_child(relative)
            ctx.watch(directory)
            if not directory.is_dir:
                fail("Area path %r is not a repository directory" % path)
            continue

        # A pattern: `*` is allowed in the last segment only, and matches
        # within that segment. The parent must be a directory with at least
        # one matching entry.
        parent_relative, _, pattern = relative.rpartition("/")
        if "*" in parent_relative:
            fail("Area path %r has a wildcard outside its last segment" % path)
        parent = root.get_child(parent_relative)
        ctx.watch(parent)
        if not parent.is_dir:
            fail("Area path %r is not under a repository directory" % path)
        matched = False
        for entry in parent.readdir():
            if _segment_matches(pattern, entry.basename):
                matched = True
                break
        if not matched:
            fail("Area path %r matches nothing in its directory" % path)
    ctx.symlink(areas, "areas.yaml")
    ctx.file("BUILD.bazel", 'exports_files(["areas.yaml"])\n')

_governance_areas = repository_rule(
    implementation = _governance_areas_impl,
    attrs = {
        "areas": attr.label(default = Label("//governance:areas.yaml"), allow_single_file = True),
        "root": attr.label(default = Label("//:MODULE.bazel"), allow_single_file = True),
        "yq": attr.label(mandatory = True, allow_single_file = True),
    },
    local = True,
)

def _governance_impl(ctx):
    # Repository rules run before toolchain resolution, so yq must run on
    # Bazel's host. Depend on its binary directly to fetch it before use.
    yq_platform_repo(
        name = "governance_yq",
        platform = repo_utils.platform(ctx),
        version = DEFAULT_YQ_VERSION,
    )
    _governance_areas(
        name = "governance_areas",
        yq = "@governance_yq//:yq.exe" if repo_utils.is_windows(ctx) else "@governance_yq//:yq",
    )

governance = module_extension(
    implementation = _governance_impl,
    arch_dependent = True,
    os_dependent = True,
)
