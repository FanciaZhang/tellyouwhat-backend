import Foundation
import HTTPTypes
import OpenAPIRuntime
import Testing
@testable import HealthAPI

@Suite("Health API transport")
struct HealthAPITransportTests {
    @Test("generated activation request has typed headers and no compatibility body")
    func generatedActivationRequest() async throws {
        let requestID = UUID()
        let recorder = RequestRecorder()
        let transport = HealthAPITransport { request in
            await recorder.record(request)
            return HealthAPIHTTPResponse(
                statusCode: 200,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"status":"active","expiresAt":"2026-09-01T00:00:00Z"}"#.utf8))
            )
        }
        let client = Client(
            serverURL: URL(string: "https://api.health.example")!,
            transport: transport
        )

        _ = try await client.activateDevelopmentEntitlement(headers: .init(
            xHealthRequestID: requestID.uuidString.lowercased(),
            xHealthDevActivation: "development-secret"
        ))

        let request = try #require(await recorder.request)
        #expect(request.operationID == Operations.ActivateDevelopmentEntitlement.id)
        #expect(request.method == "POST")
        #expect(request.path == "/v1/dev/entitlements/activate")
        #expect(request.body.isEmpty)
        #expect(request.header(named: "X-Health-Request-ID") == requestID.uuidString.lowercased())
        #expect(request.header(named: "X-Health-Dev-Activation") == "development-secret")
    }

    @Test("streaming response remains incremental")
    func streamingResponse() async throws {
        let responseStream = AsyncThrowingStream<Data, any Error> { continuation in
            continuation.yield(Data("first".utf8))
            continuation.yield(Data("second".utf8))
            continuation.finish()
        }
        let transport = HealthAPITransport { _ in
            HealthAPIHTTPResponse(statusCode: 200, body: .stream(responseStream))
        }
        let response = try await transport.send(
            .init(method: .get, scheme: nil, authority: nil, path: "/healthz"),
            body: nil,
            baseURL: URL(string: "https://api.health.example")!,
            operationID: Operations.GetHealth.id
        )
        let body = try #require(response.1)
        var chunks: [String] = []
        for try await chunk in body {
            chunks.append(String(decoding: chunk, as: UTF8.self))
        }
        #expect(chunks == ["first", "second"])
    }

    @Test("job capability and enqueue serialize the same request bytes")
    func jobBodyBytesAreStable() async throws {
        let recorder = RequestRecorder()
        let transport = HealthAPITransport { request in
            await recorder.record(request)
            let payload: String
            let statusCode: Int
            switch request.operationID {
            case Operations.IssueAIJobCapability.id:
                statusCode = 201
                payload = #"{"jobID":"4d88fba4-48f1-447d-a72a-af0449ad90b3","token":"one-time-capability","expiresAt":"2026-09-01T00:00:00Z"}"#
            case Operations.EnqueueAIJob.id:
                statusCode = 202
                payload = #"{"jobID":"4d88fba4-48f1-447d-a72a-af0449ad90b3","requestID":"6d4e5a47-4c66-42d0-81c0-8d12c4425239","status":"queued","createdAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-01T00:00:00Z"}"#
            default:
                throw UnexpectedOperationError(operationID: request.operationID)
            }
            return HealthAPIHTTPResponse(
                statusCode: statusCode,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(payload.utf8))
            )
        }
        let client = Client(
            serverURL: URL(string: "https://api.health.example")!,
            transport: transport
        )
        let request = try sampleAIRequest()

        _ = try await client.issueAIJobCapability(
            headers: .init(xHealthRequestID: request.requestID),
            body: .json(request)
        )
        _ = try await client.enqueueAIJob(
            headers: .init(
                xHealthRequestID: request.requestID,
                xHealthJobID: "4d88fba4-48f1-447d-a72a-af0449ad90b3",
                xHealthJobCapability: "one-time-capability"
            ),
            body: .json(request)
        )

        let requests = await recorder.requests
        #expect(requests.count == 2)
        let capabilityRequest = try #require(requests.first)
        let enqueueRequest = try #require(requests.last)
        #expect(capabilityRequest.body == enqueueRequest.body)
        #expect(capabilityRequest.path == "/v1/ai/job-capabilities")
        #expect(enqueueRequest.path == "/v1/ai/jobs")
        #expect(enqueueRequest.header(named: "X-Health-Job-ID") == "4d88fba4-48f1-447d-a72a-af0449ad90b3")
        #expect(enqueueRequest.header(named: "X-Health-Job-Capability") == "one-time-capability")
        #expect(enqueueRequest.header(named: "Authorization") == nil)
    }

    @Test("generated client decodes non-success responses into typed output")
    func typedErrorOutput() async throws {
        let transport = HealthAPITransport { _ in
            HealthAPIHTTPResponse(
                statusCode: 429,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"error":{"code":"daily_quota_exceeded","message":"limit","requestID":"request-1"}}"#.utf8))
            )
        }
        let client = Client(
            serverURL: URL(string: "https://api.health.example")!,
            transport: transport
        )

        let output = try await client.completeAIRequest(
            headers: .init(xHealthRequestID: "6d4e5a47-4c66-42d0-81c0-8d12c4425239"),
            body: .json(try sampleAIRequest())
        )

        guard case .tooManyRequests(let response) = output else {
            Issue.record("应将 429 解码为生成的 tooManyRequests 分支")
            return
        }
        let error = try response.body.json
        #expect(error.error.code == "daily_quota_exceeded")
        #expect(error.error.requestID == "request-1")
    }

    @Test("request collection stops before exceeding the configured limit")
    func requestBodyLimit() async {
        let recorder = RequestRecorder()
        let transport = HealthAPITransport(maximumRequestBodyBytes: 3) { request in
            await recorder.record(request)
            return HealthAPIHTTPResponse(statusCode: 204, body: .data(Data()))
        }

        var didThrow = false
        do {
            _ = try await transport.send(
                .init(method: .post, scheme: nil, authority: nil, path: "/v1/ai/complete"),
                body: HTTPBody(Data([0x01, 0x02, 0x03, 0x04])),
                baseURL: URL(string: "https://api.health.example")!,
                operationID: Operations.CompleteAIRequest.id
            )
            Issue.record("超过上限的请求体不应交给发送器")
        } catch {
            didThrow = true
            // The runtime owns the concrete collection error. The contract we
            // require here is that the sender never sees a partial body.
        }
        #expect(didThrow)
        #expect(await recorder.requests.isEmpty)
    }

    @Test("invalid response header names are rejected")
    func invalidResponseHeaderName() async {
        let transport = HealthAPITransport { _ in
            HealthAPIHTTPResponse(
                statusCode: 200,
                headers: [.init(name: "invalid\nheader", value: "value")],
                body: .data(Data())
            )
        }

        do {
            _ = try await transport.send(
                .init(method: .get, scheme: nil, authority: nil, path: "/healthz"),
                body: nil,
                baseURL: URL(string: "https://api.health.example")!,
                operationID: Operations.GetHealth.id
            )
            Issue.record("非法响应头不应进入生成客户端")
        } catch let error as HealthAPITransportError {
            #expect(error == .invalidResponseHeaderName("invalid\nheader"))
        } catch {
            Issue.record("返回了错误类型：\(error)")
        }
    }

    private func sampleAIRequest() throws -> Components.Schemas.AIRequest {
        try JSONDecoder().decode(
            Components.Schemas.AIRequest.self,
            from: Data(#"{"requestID":"6d4e5a47-4c66-42d0-81c0-8d12c4425239","operation":"meal_text_capture","contractVersion":"ai-request-v1","promptVersion":"meal-text-v1","prompt":"记录一碗面","responseSchema":{},"options":{"stream":false},"media":[],"semanticSignature":"meal-text"}"#.utf8)
        )
    }
}

private actor RequestRecorder {
    private(set) var requests: [HealthAPIHTTPRequest] = []

    var request: HealthAPIHTTPRequest? {
        requests.last
    }

    func record(_ request: HealthAPIHTTPRequest) {
        requests.append(request)
    }
}

private struct UnexpectedOperationError: Error {
    let operationID: String
}
