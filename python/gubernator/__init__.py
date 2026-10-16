import time

import grpc

from . import gubernator_pb2_grpc as pb_grpc

MILLISECOND = 1
SECOND = MILLISECOND * 1000
MINUTE = SECOND * 60


def sleep_until_reset(reset_time):
    """Sleep until `reset_time`, a unix timestamp in milliseconds as returned in a RateLimitResp."""
    time.sleep(max(0.0, reset_time / 1000 - time.time()))


def V1Client(endpoint='127.0.0.1:9090'):
    channel = grpc.insecure_channel(endpoint)
    return pb_grpc.V1Stub(channel)
