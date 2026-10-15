import Foundation
import HTTPTypes
import OpenAPIRuntime

/// Namespace marker for the generated Album account storage API client.
public enum AlbumAPIContract {}

/// A serialized request produced by the generated client.
public struct AlbumAPIHTTPRequest: Sendable, Equatable {
    public struct Header: Sendable, Equatable {
        public let name: String
        public let value: String

        public init(name: String, value: String) {
            self.name = name
            self.value = value
        }
    }

    public let method: String
    public let path: String
    public let headers: [Header]
    public let body: Data
    public let baseURL: URL
    public let operationID: String

    public init(
        method: String,
        path: String,
        headers: [Header],
        body: Data,
        baseURL: URL,
        operationID: String
    ) {
        self.method = method
        self.path = path
        self.headers = headers
        self.body = body
        self.baseURL = baseURL
        self.operationID = operationID
    }

    public func header(named name: String) -> String? {
        headers.first { $0.name.caseInsensitiveCompare(name) == .orderedSame }?.value
    }
}

/// A response supplied to the generated client by the platform transport.
public struct AlbumAPIHTTPResponse: Sendable {
    public enum Body: Sendable {
        case data(Data)
    }

    public let statusCode: Int
    public let headers: [AlbumAPIHTTPRequest.Header]
    public let body: Body

    public init(
        statusCode: Int,
        headers: [AlbumAPIHTTPRequest.Header] = [],
        body: Body
    ) {
        self.statusCode = statusCode
        self.headers = headers
        self.body = body
    }
}

public enum AlbumAPITransportError: Error, Sendable, Equatable {
    case missingRequestPath
    case invalidResponseHeaderName(String)
}

/// Bridges the generated OpenAPI client to a platform-owned HTTP sender.
///
/// Authentication is supplied by the app's account transport. App Attest or a
/// device ID alone must not be used as an album account token. Media bytes use
/// separate file upload tasks; this bridge carries only bounded control JSON.
public struct AlbumAPITransport: ClientTransport {
    public typealias Sender = @Sendable (AlbumAPIHTTPRequest) async throws -> AlbumAPIHTTPResponse

    private let maximumRequestBodyBytes: Int
    private let sender: Sender

    public init(
        maximumRequestBodyBytes: Int = 2 * 1024 * 1024,
        sender: @escaping Sender
    ) {
        self.maximumRequestBodyBytes = maximumRequestBodyBytes
        self.sender = sender
    }

    public func send(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String
    ) async throws -> (HTTPResponse, HTTPBody?) {
        guard let path = request.path else {
            throw AlbumAPITransportError.missingRequestPath
        }
        let requestBody: Data
        if let body {
            requestBody = try await Data(collecting: body, upTo: maximumRequestBodyBytes)
        } else {
            requestBody = Data()
        }
        let serializedRequest = AlbumAPIHTTPRequest(
            method: request.method.rawValue,
            path: path,
            headers: request.headerFields.map {
                .init(name: $0.name.canonicalName, value: $0.value)
            },
            body: requestBody,
            baseURL: baseURL,
            operationID: operationID
        )
        let serializedResponse = try await sender(serializedRequest)
        var responseHeaders = HTTPFields()
        for header in serializedResponse.headers {
            guard let name = HTTPField.Name(header.name) else {
                throw AlbumAPITransportError.invalidResponseHeaderName(header.name)
            }
            responseHeaders.append(HTTPField(name: name, value: header.value))
        }
        let response = HTTPResponse(
            status: .init(code: serializedResponse.statusCode),
            headerFields: responseHeaders
        )
        switch serializedResponse.body {
        case .data(let data):
            return (response, HTTPBody(data))
        }
    }
}
