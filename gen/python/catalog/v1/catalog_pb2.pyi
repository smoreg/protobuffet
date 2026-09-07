from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Iterable as _Iterable, Mapping as _Mapping, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ProductStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PRODUCT_STATUS_UNSPECIFIED: _ClassVar[ProductStatus]
    PRODUCT_STATUS_DRAFT: _ClassVar[ProductStatus]
    PRODUCT_STATUS_ACTIVE: _ClassVar[ProductStatus]
    PRODUCT_STATUS_ARCHIVED: _ClassVar[ProductStatus]
PRODUCT_STATUS_UNSPECIFIED: ProductStatus
PRODUCT_STATUS_DRAFT: ProductStatus
PRODUCT_STATUS_ACTIVE: ProductStatus
PRODUCT_STATUS_ARCHIVED: ProductStatus

class Product(_message.Message):
    __slots__ = ("id", "sku", "title", "price_minor", "currency", "status", "created_at")
    ID_FIELD_NUMBER: _ClassVar[int]
    SKU_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    PRICE_MINOR_FIELD_NUMBER: _ClassVar[int]
    CURRENCY_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    id: str
    sku: str
    title: str
    price_minor: int
    currency: str
    status: ProductStatus
    created_at: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[str] = ..., sku: _Optional[str] = ..., title: _Optional[str] = ..., price_minor: _Optional[int] = ..., currency: _Optional[str] = ..., status: _Optional[_Union[ProductStatus, str]] = ..., created_at: _Optional[_Union[_timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class GetProductRequest(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: str
    def __init__(self, id: _Optional[str] = ...) -> None: ...

class GetProductResponse(_message.Message):
    __slots__ = ("product",)
    PRODUCT_FIELD_NUMBER: _ClassVar[int]
    product: Product
    def __init__(self, product: _Optional[_Union[Product, _Mapping]] = ...) -> None: ...

class ListProductsRequest(_message.Message):
    __slots__ = ("page_size", "page_token", "status")
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    page_size: int
    page_token: str
    status: ProductStatus
    def __init__(self, page_size: _Optional[int] = ..., page_token: _Optional[str] = ..., status: _Optional[_Union[ProductStatus, str]] = ...) -> None: ...

class ListProductsResponse(_message.Message):
    __slots__ = ("products", "next_page_token")
    PRODUCTS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    products: _containers.RepeatedCompositeFieldContainer[Product]
    next_page_token: str
    def __init__(self, products: _Optional[_Iterable[_Union[Product, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...
