# -*- coding: utf-8 -*-
# Generated by the protocol buffer compiler.  DO NOT EDIT!
# NO CHECKED-IN PROTOBUF GENCODE
# source: gubernator.proto
# Protobuf Python Version: 6.30.1
"""Generated protocol buffer code."""
from google.protobuf import descriptor as _descriptor
from google.protobuf import descriptor_pool as _descriptor_pool
from google.protobuf import runtime_version as _runtime_version
from google.protobuf import symbol_database as _symbol_database
from google.protobuf.internal import builder as _builder
_runtime_version.ValidateProtobufRuntimeVersion(
    _runtime_version.Domain.PUBLIC,
    6,
    30,
    1,
    '',
    'gubernator.proto'
)
# @@protoc_insertion_point(imports)

_sym_db = _symbol_database.Default()


from google.api import annotations_pb2 as google_dot_api_dot_annotations__pb2


DESCRIPTOR = _descriptor_pool.Default().AddSerializedFile(b'\n\x10gubernator.proto\x12\rpb.gubernator\x1a\x1cgoogle/api/annotations.proto\"K\n\x10GetRateLimitsReq\x12\x37\n\x08requests\x18\x01 \x03(\x0b\x32\x1b.pb.gubernator.RateLimitReqR\x08requests\"O\n\x11GetRateLimitsResp\x12:\n\tresponses\x18\x01 \x03(\x0b\x32\x1c.pb.gubernator.RateLimitRespR\tresponses\"\xc1\x03\n\x0cRateLimitReq\x12\x12\n\x04name\x18\x01 \x01(\tR\x04name\x12\x1d\n\nunique_key\x18\x02 \x01(\tR\tuniqueKey\x12\x12\n\x04hits\x18\x03 \x01(\x03R\x04hits\x12\x14\n\x05limit\x18\x04 \x01(\x03R\x05limit\x12\x1a\n\x08\x64uration\x18\x05 \x01(\x03R\x08\x64uration\x12\x36\n\talgorithm\x18\x06 \x01(\x0e\x32\x18.pb.gubernator.AlgorithmR\talgorithm\x12\x33\n\x08\x62\x65havior\x18\x07 \x01(\x0e\x32\x17.pb.gubernator.BehaviorR\x08\x62\x65havior\x12\x14\n\x05\x62urst\x18\x08 \x01(\x03R\x05\x62urst\x12\x45\n\x08metadata\x18\t \x03(\x0b\x32).pb.gubernator.RateLimitReq.MetadataEntryR\x08metadata\x12\"\n\ncreated_at\x18\n \x01(\x03H\x00R\tcreatedAt\x88\x01\x01\x1a;\n\rMetadataEntry\x12\x10\n\x03key\x18\x01 \x01(\tR\x03key\x12\x14\n\x05value\x18\x02 \x01(\tR\x05value:\x02\x38\x01\x42\r\n\x0b_created_at\"\xac\x02\n\rRateLimitResp\x12-\n\x06status\x18\x01 \x01(\x0e\x32\x15.pb.gubernator.StatusR\x06status\x12\x14\n\x05limit\x18\x02 \x01(\x03R\x05limit\x12\x1c\n\tremaining\x18\x03 \x01(\x03R\tremaining\x12\x1d\n\nreset_time\x18\x04 \x01(\x03R\tresetTime\x12\x14\n\x05\x65rror\x18\x05 \x01(\tR\x05\x65rror\x12\x46\n\x08metadata\x18\x06 \x03(\x0b\x32*.pb.gubernator.RateLimitResp.MetadataEntryR\x08metadata\x1a;\n\rMetadataEntry\x12\x10\n\x03key\x18\x01 \x01(\tR\x03key\x12\x14\n\x05value\x18\x02 \x01(\tR\x05value:\x02\x38\x01\"\x10\n\x0eHealthCheckReq\"\x8f\x01\n\x0fHealthCheckResp\x12\x16\n\x06status\x18\x01 \x01(\tR\x06status\x12\x18\n\x07message\x18\x02 \x01(\tR\x07message\x12\x1d\n\npeer_count\x18\x03 \x01(\x05R\tpeerCount\x12+\n\x11\x61\x64vertise_address\x18\x04 \x01(\tR\x10\x61\x64vertiseAddress\"\x0e\n\x0cLiveCheckReq\"\x0f\n\rLiveCheckResp*/\n\tAlgorithm\x12\x10\n\x0cTOKEN_BUCKET\x10\x00\x12\x10\n\x0cLEAKY_BUCKET\x10\x01*\x8d\x01\n\x08\x42\x65havior\x12\x0c\n\x08\x42\x41TCHING\x10\x00\x12\x0f\n\x0bNO_BATCHING\x10\x01\x12\n\n\x06GLOBAL\x10\x02\x12\x19\n\x15\x44URATION_IS_GREGORIAN\x10\x04\x12\x13\n\x0fRESET_REMAINING\x10\x08\x12\x10\n\x0cMULTI_REGION\x10\x10\x12\x14\n\x10\x44RAIN_OVER_LIMIT\x10 *)\n\x06Status\x12\x0f\n\x0bUNDER_LIMIT\x10\x00\x12\x0e\n\nOVER_LIMIT\x10\x01\x32\xbc\x02\n\x02V1\x12p\n\rGetRateLimits\x12\x1f.pb.gubernator.GetRateLimitsReq\x1a .pb.gubernator.GetRateLimitsResp\"\x1c\x82\xd3\xe4\x93\x02\x16\"\x11/v1/GetRateLimits:\x01*\x12\x65\n\x0bHealthCheck\x12\x1d.pb.gubernator.HealthCheckReq\x1a\x1e.pb.gubernator.HealthCheckResp\"\x17\x82\xd3\xe4\x93\x02\x11\x12\x0f/v1/HealthCheck\x12]\n\tLiveCheck\x12\x1b.pb.gubernator.LiveCheckReq\x1a\x1c.pb.gubernator.LiveCheckResp\"\x15\x82\xd3\xe4\x93\x02\x0f\x12\r/v1/LiveCheckB(Z#github.com/gubernator-io/gubernator\x80\x01\x01\x62\x06proto3')

_globals = globals()
_builder.BuildMessageAndEnumDescriptors(DESCRIPTOR, _globals)
_builder.BuildTopDescriptorsAndMessages(DESCRIPTOR, 'gubernator_pb2', _globals)
if not _descriptor._USE_C_DESCRIPTORS:
  _globals['DESCRIPTOR']._loaded_options = None
  _globals['DESCRIPTOR']._serialized_options = b'Z#github.com/gubernator-io/gubernator\200\001\001'
  _globals['_RATELIMITREQ_METADATAENTRY']._loaded_options = None
  _globals['_RATELIMITREQ_METADATAENTRY']._serialized_options = b'8\001'
  _globals['_RATELIMITRESP_METADATAENTRY']._loaded_options = None
  _globals['_RATELIMITRESP_METADATAENTRY']._serialized_options = b'8\001'
  _globals['_V1'].methods_by_name['GetRateLimits']._loaded_options = None
  _globals['_V1'].methods_by_name['GetRateLimits']._serialized_options = b'\202\323\344\223\002\026\"\021/v1/GetRateLimits:\001*'
  _globals['_V1'].methods_by_name['HealthCheck']._loaded_options = None
  _globals['_V1'].methods_by_name['HealthCheck']._serialized_options = b'\202\323\344\223\002\021\022\017/v1/HealthCheck'
  _globals['_V1'].methods_by_name['LiveCheck']._loaded_options = None
  _globals['_V1'].methods_by_name['LiveCheck']._serialized_options = b'\202\323\344\223\002\017\022\r/v1/LiveCheck'
  _globals['_ALGORITHM']._serialized_start=1175
  _globals['_ALGORITHM']._serialized_end=1222
  _globals['_BEHAVIOR']._serialized_start=1225
  _globals['_BEHAVIOR']._serialized_end=1366
  _globals['_STATUS']._serialized_start=1368
  _globals['_STATUS']._serialized_end=1409
  _globals['_GETRATELIMITSREQ']._serialized_start=65
  _globals['_GETRATELIMITSREQ']._serialized_end=140
  _globals['_GETRATELIMITSRESP']._serialized_start=142
  _globals['_GETRATELIMITSRESP']._serialized_end=221
  _globals['_RATELIMITREQ']._serialized_start=224
  _globals['_RATELIMITREQ']._serialized_end=673
  _globals['_RATELIMITREQ_METADATAENTRY']._serialized_start=599
  _globals['_RATELIMITREQ_METADATAENTRY']._serialized_end=658
  _globals['_RATELIMITRESP']._serialized_start=676
  _globals['_RATELIMITRESP']._serialized_end=976
  _globals['_RATELIMITRESP_METADATAENTRY']._serialized_start=599
  _globals['_RATELIMITRESP_METADATAENTRY']._serialized_end=658
  _globals['_HEALTHCHECKREQ']._serialized_start=978
  _globals['_HEALTHCHECKREQ']._serialized_end=994
  _globals['_HEALTHCHECKRESP']._serialized_start=997
  _globals['_HEALTHCHECKRESP']._serialized_end=1140
  _globals['_LIVECHECKREQ']._serialized_start=1142
  _globals['_LIVECHECKREQ']._serialized_end=1156
  _globals['_LIVECHECKRESP']._serialized_start=1158
  _globals['_LIVECHECKRESP']._serialized_end=1173
  _globals['_V1']._serialized_start=1412
  _globals['_V1']._serialized_end=1728
# @@protoc_insertion_point(module_scope)
