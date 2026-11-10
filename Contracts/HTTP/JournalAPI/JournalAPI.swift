import Foundation
import HTTPTypes
import OpenAPIRuntime

/// Namespace marker for the generated Journal managed-AI API client.
public enum JournalAPIContract {}

/// Large JSON bodies require an image or published voice-catalog operation and
/// its exact route, never merely a caller-supplied operation name.
public enum JournalAPIBodyLimits {
    public static let imageJSON = 34_000_000
    public static let ordinaryResponse = 1_048_576
    public static let voiceCatalog = 32 * 1024 * 1024
    public static let voiceOrganization = 8 * 1024 * 1024

    public static func isReadAloudStream(method: String, path: String, operationID: String) -> Bool {
        let route = path.split(separator: "?", maxSplits: 1, omittingEmptySubsequences: false)[0]
        let parts = route.split(separator: "/", omittingEmptySubsequences: false)
        guard method == "GET" && operationID == Operations.StreamJournalReadAloudSynthesis.id
            && parts.count == 7 && parts[0].isEmpty && parts[1] == "v1" && parts[2] == "journal"
            && parts[3] == "read-aloud" && parts[4] == "syntheses" && parts[6] == "events",
              let id = UUID(uuidString: String(parts[5])) else { return false }
        return id.uuidString.lowercased() == parts[5]
    }

    public static func request(method: String, path: String, operationID: String, fallback: Int) -> Int {
        if method == "POST" && path == "/v1/journal/voice/organizations"
            && operationID == Operations.CreateJournalVoiceOrganization.id { return 3 * 1024 * 1024 }
        return method == "POST" && path == "/v1/journal/illustrations"
            && operationID == Operations.CreateJournalIllustration.id ? imageJSON : fallback
    }

    public static func response(method: String, path: String, operationID: String, status: Int) -> Int {
        if status == 200 && method == "POST" && path == "/v1/journal/voice/organizations"
            && operationID == Operations.CreateJournalVoiceOrganization.id { return voiceOrganization }
        if status == 200 && method == "GET" && path == "/v1/journal/read-aloud/voices"
            && operationID == Operations.GetJournalReadAloudVoices.id { return voiceCatalog }
        let parts = path.split(separator: "/", omittingEmptySubsequences: false)
        if status == 200, method == "GET", operationID == Operations.GetJournalVoiceOrganization.id,
           parts.count == 6, parts[0].isEmpty, parts[1] == "v1", parts[2] == "journal", parts[3] == "voice",
           parts[4] == "organizations", let id = UUID(uuidString: String(parts[5])), id.uuidString.lowercased() == parts[5] {
            return voiceOrganization
        }
        guard status == 200, method == "GET", operationID == Operations.GetJournalIllustrationResult.id,
              parts.count == 6, parts[0].isEmpty, parts[1] == "v1", parts[2] == "journal",
              parts[3] == "illustrations", parts[5] == "result",
              let id = UUID(uuidString: String(parts[4])), id.uuidString.lowercased() == parts[4] else {
            return ordinaryResponse
        }
        return imageJSON
    }
}

/// A serialized request produced by the generated client.
public struct JournalAPIHTTPRequest: Sendable, Equatable {
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
public struct JournalAPIHTTPResponse: Sendable {
    public enum Body: Sendable {
        case data(Data)
        case stream(AsyncThrowingStream<Data, any Error>)
        case httpBody(HTTPBody)
    }

    public let statusCode: Int
    public let headers: [JournalAPIHTTPRequest.Header]
    public let body: Body

    public init(
        statusCode: Int,
        headers: [JournalAPIHTTPRequest.Header] = [],
        body: Body
    ) {
        self.statusCode = statusCode
        self.headers = headers
        self.body = body
    }
}

public enum JournalAPITransportError: Error, Sendable, Equatable {
    case missingRequestPath
    case invalidResponseHeaderName(String)
    case invalidRequestHeaderValue(String)
}

/// Bridges the generated OpenAPI client to a platform-owned HTTP sender.
///
/// The bridge exposes the exact generated method, path, headers, and body so
/// App Attest signs the same serialized bytes that the backend authenticates.
public struct JournalAPITransport: ClientTransport {
    public typealias Sender = @Sendable (JournalAPIHTTPRequest) async throws -> JournalAPIHTTPResponse

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
            throw JournalAPITransportError.missingRequestPath
        }
        let requestBody: Data
        if let body {
            requestBody = try await Data(collecting: body, upTo: JournalAPIBodyLimits.request(
                method: request.method.rawValue, path: path, operationID: operationID, fallback: maximumRequestBodyBytes))
        } else {
            requestBody = Data()
        }
        let serializedRequest = JournalAPIHTTPRequest(
            method: request.method.rawValue,
            path: path,
            headers: try request.headerFields.map { field in
                var value = field.value
                if request.method == .get, path == "/v1/journal/read-aloud/voices",
                   operationID == Operations.GetJournalReadAloudVoices.id, field.name == .ifNoneMatch {
                    // The runtime URI-escapes header strings; HTTP entity tags retain their quotes.
                    guard let decoded = value.removingPercentEncoding else {
                        throw JournalAPITransportError.invalidRequestHeaderValue("If-None-Match")
                    }
                    let tag = decoded.hasPrefix("W/") ? String(decoded.dropFirst(2)) : decoded
                    guard decoded.utf8.count <= 512, tag.hasPrefix("\""), tag.hasSuffix("\""), tag.utf8.count >= 2,
                          tag.utf8.dropFirst().dropLast().allSatisfy({ $0 == 33 || (35...126).contains($0) }) else {
                        throw JournalAPITransportError.invalidRequestHeaderValue("If-None-Match")
                    }
                    value = decoded
                }
                return .init(name: field.name.canonicalName, value: value)
            },
            body: requestBody,
            baseURL: baseURL,
            operationID: operationID
        )
        let serializedResponse = try await sender(serializedRequest)
        var responseHeaders = HTTPFields()
        for header in serializedResponse.headers {
            guard let name = HTTPField.Name(header.name) else {
                throw JournalAPITransportError.invalidResponseHeaderName(header.name)
            }
            responseHeaders.append(HTTPField(name: name, value: header.value))
        }
        let response = HTTPResponse(
            status: .init(code: serializedResponse.statusCode),
            headerFields: responseHeaders
        )
        switch serializedResponse.body {
        case .httpBody(let body):
            return (response, body)
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
