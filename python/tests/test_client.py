# Copyright 2018-2022 Mailgun Technologies Inc
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

import os
import shutil
import signal
import subprocess
import sys
import time

import pytest

import gubernator
from gubernator import gubernator_pb2 as pb

REPO_ROOT = os.path.join(os.path.dirname(__file__), "..", "..")
ENDPOINT = "127.0.0.1:9990"


@pytest.fixture(scope="module")
def cluster(tmp_path_factory):
    if shutil.which("go") is None:
        pytest.skip("go is required to start a local gubernator cluster")

    # Build first; `go run` would leave the cluster binary running after we stop it.
    binary = str(tmp_path_factory.mktemp("cluster") / "gubernator-cluster")
    subprocess.run(["go", "build", "-o", binary, "./cmd/gubernator-cluster"],
                   cwd=REPO_ROOT, check=True)

    proc = subprocess.Popen([binary], stdout=subprocess.PIPE)
    try:
        for line in iter(proc.stdout.readline, b""):
            if b"Running" in line:
                break
        else:
            pytest.fail("cluster exited before it was ready")
        yield proc
    finally:
        proc.send_signal(signal.SIGINT)
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            # Graceful shutdown has no deadline, so a hung cluster would hold the ports.
            proc.kill()
            proc.wait()


def test_import_exposes_client():
    assert callable(gubernator.V1Client)
    assert callable(gubernator.sleep_until_reset)


@pytest.mark.parametrize("module", [
    "gubernator",
    "gubernator.gubernator_pb2",
    "gubernator.gubernator_pb2_grpc",
    "gubernator.peers_pb2",
    "gubernator.peers_pb2_grpc",
])
def test_generated_stubs_import_as_package(module, tmp_path):
    # Fresh interpreter outside the source tree so a sibling-directory sys.path entry cannot mask a bare import.
    result = subprocess.run([sys.executable, "-c", "import " + module],
                            cwd=tmp_path, capture_output=True, text=True)

    assert result.returncode == 0, result.stderr


def test_client_stub_has_rpcs():
    client = gubernator.V1Client(ENDPOINT)

    assert callable(client.GetRateLimits)
    assert callable(client.HealthCheck)


def test_sleep_until_reset_past_returns_immediately():
    reset_time = int(time.time() * 1000) - 60 * gubernator.SECOND

    start = time.monotonic()
    gubernator.sleep_until_reset(reset_time)

    assert time.monotonic() - start < 0.5


def test_sleep_until_reset_waits_for_reset_time():
    reset_time = int(time.time() * 1000) + 1500

    start = time.monotonic()
    gubernator.sleep_until_reset(reset_time)

    assert 1.0 <= time.monotonic() - start < 3.0


def test_sleep_until_reset_waits_sub_second():
    reset_time = int(time.time() * 1000) + 600

    start = time.monotonic()
    gubernator.sleep_until_reset(reset_time)

    assert 0.3 <= time.monotonic() - start < 1.5


def test_health_check(cluster):
    resp = gubernator.V1Client(ENDPOINT).HealthCheck(pb.HealthCheckReq(), timeout=5)

    assert resp.status == "healthy"


def test_get_rate_limits(cluster):
    req = pb.GetRateLimitsReq(requests=[pb.RateLimitReq(
        name="test-ns",
        unique_key="domain-id-0001",
        algorithm=pb.TOKEN_BUCKET,
        duration=gubernator.SECOND * 2,
        limit=10,
        hits=1,
    )])

    resp = gubernator.V1Client(ENDPOINT).GetRateLimits(req, timeout=5)

    assert len(resp.responses) == 1
    assert resp.responses[0].status == pb.UNDER_LIMIT
    assert resp.responses[0].remaining == 9


def test_sleep_until_reset_with_response_reset_time(cluster):
    req = pb.GetRateLimitsReq(requests=[pb.RateLimitReq(
        name="test-ns",
        unique_key="domain-id-0002",
        algorithm=pb.TOKEN_BUCKET,
        duration=gubernator.SECOND,
        limit=1,
        hits=1,
    )])
    resp = gubernator.V1Client(ENDPOINT).GetRateLimits(req, timeout=5)

    start = time.monotonic()
    gubernator.sleep_until_reset(resp.responses[0].reset_time)

    assert time.monotonic() - start < 2.0
    assert int(time.time() * 1000) >= resp.responses[0].reset_time
