# Copyright 2025 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at

#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import argparse
import os
import shlex
import subprocess
import sys

ROOT_CMAKE_DIR = '/cuda-samples/build'
SAMPLES_DIR = ROOT_CMAKE_DIR + '/Samples'

parser = argparse.ArgumentParser()

parser.add_argument(
    '--test-dir',
    help='Test to run. This should be someething like "0_Introduction/UnifiedMemoryStreams"',
    required=True,
    type=str,
)

def run_cmake():
    os.makedirs(ROOT_CMAKE_DIR, exist_ok=True)
    wd = os.getcwd()
    os.chdir(ROOT_CMAKE_DIR)
    args = shlex.split('cmake ..')
    subprocess.run(
        args=args,
        check=True,
    )
    os.chdir(wd)

def run_test(test_dir: str=''):
    make_path = os.path.join(SAMPLES_DIR, test_dir)
    args = shlex.split(f'make -C {make_path}')
    subprocess.run(
        args=args,
        check=True,
    )
    test_path = os.path.join(make_path,os.path.basename(test_dir))
    subprocess.run(
        args=[test_path],
        check=True,
    )


if __name__ == '__main__':
    args = parser.parse_args(sys.argv[1:])
    run_cmake()
    run_test(args.test_dir)

    