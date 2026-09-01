import Foundation
import HTTPTypes
import OpenAPIRuntime

/// Namespace marker for the generated Health managed-AI API client.
public enum HealthAPIContract {}

/// A serialized request produced by the generated client.
public struct HealthAPIHTTPRequest: Sendable, Equatable {
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
public struct HealthAPIHTTPResponse: Sendable {
    public enum Body: Sendable {
        case data(Data)
        case stream(AsyncThrowingStream<Data, any Error>)
    }

    public let statusCode: Int
    public let headers: [HealthAPIHTTPRequest.Header]
    public let body: Body

    public init(
        statusCode: Int,
        headers: [HealthAPIHTTPRequest.Header] = [],
        body: Body
    ) {
        self.statusCode = statusCode
        self.headers = headers
        self.body = body
    }
}

public enum HealthAPITransportError: Error, Sendable, Equatable {
    case missingRequestPath
    case invalidResponseHeaderName(String)
}

/// Bridges the generated OpenAPI client to a platform-owned HTTP sender.
///
/// The bridge exposes the exact generated method, path, headers, and body. This
/// lets the app bind App Attest to the serialized bytes and lets background
/// URLSession capture the generated request without duplicating route strings.
public struct HealthAPITransport: ClientTransport {
    public typealias Sender = @Sendable (HealthAPIHTTPRequest) async throws -> HealthAPIHTTPResponse

    private let maximumRequestBodyBytes: Int
    private let sender: Sender

    public init(
        maximumRequestBodyBytes: Int = 16 * 1024 * 1024,
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
            throw HealthAPITransportError.missingRequestPath
        }
        let requestBody: Data
        if let body {
            requestBody = try await Data(collecting: body, upTo: maximumRequestBodyBytes)
        } else {
            requestBody = Data()
        }
        let serializedRequest = HealthAPIHTTPRequest(
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
                throw HealthAPITransportError.invalidResponseHeaderName(header.name)
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
        case .stream(let stream):
            let byteStream = AsyncThrowingStream<HTTPBody.ByteChunk, any Error> { continuation in
                let task = Task {
                    do {
                        for try await data in stream {
                            try Task.checkCancellation()
                            continuation.yield(ArraySlice(data))
                        }
                        continuation.finish()
                    } catch {
                        continuation.finish(throwing: error)
                    }
                }
                continuation.onTermination = { _ in task.cancel() }
            }
            return (response, HTTPBody(byteStream, length: .unknown))
        }
    }
}
