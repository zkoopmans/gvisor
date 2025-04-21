#!/bin/bash

# Copyright 2024 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# This script outputs a sorted list of CUDA sample tests, one per line.

set -euo pipefail

mkdir -p /cuda-samples/build > /dev/null 2> /dev/null
pushd /cuda-samples/build > /dev/null 2> /dev/null
cmake .. > /dev/null 2> /dev/null
popd > /dev/null 2> /dev/null

(
  while IFS= read -r makefile_path; do
    dirname "$makefile_path"
  done < <(find /cuda-samples -type f -name Makefile) \
    | grep -vE '^/cuda-samples$' | grep -vE '/7_libNVVM'

) | sed 's~/cuda-samples/build/Samples/~~' | grep '/' | sort