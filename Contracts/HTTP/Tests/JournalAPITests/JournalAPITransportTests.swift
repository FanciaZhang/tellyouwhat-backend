import Foundation
import Testing
@testable import JournalAPI

@Suite("Journal API transport")
struct JournalAPITransportTests {
    @Test("generated organizer request owns the route, headers, and JSON body")
    func generatedOrganizerRequest() async throws {
        let requestID = UUID()
        let recorder = RequestRecorder()
        let transport = JournalAPITransport { request in
            await recorder.record(request)
            return JournalAPIHTTPResponse(
                statusCode: 200,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"requestID":"\#(requestID.uuidString.lowercased())","contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","analysisVersion":"journal-organize-v1","tags":[],"existingBookRecommendations":[],"newBookSuggestions":[],"quota":{"dailyTokensRemaining":1,"monthlyTokensRemaining":1,"available":true}}"#.utf8))
            )
        }
        let client = Client(
            serverURL: URL(string: "https://api.journal.example")!,
            transport: transport
        )
        let input = try JSONDecoder().decode(
            Components.Schemas.OrganizeRequest.self,
            from: Data(#"{"requestID":"\#(requestID.uuidString.lowercased())","contractVersion":"journal-organize-v1","contentHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","title":"今天","body":"记录正文","existingTags":[],"rejectedTagNames":[],"books":[]}"#.utf8)
        )

        _ = try await client.organizeJournal(
            headers: .init(xTellyouwhatRequestID: requestID.uuidString.lowercased()),
            body: .json(input)
        )

        let request = try #require(await recorder.request)
        #expect(request.operationID == Operations.OrganizeJournal.id)
        #expect(request.method == "POST")
        #expect(request.path == "/v1/journal/organization")
        #expect(request.header(named: "X-Tellyouwhat-Request-ID") == requestID.uuidString.lowercased())
        #expect(!request.body.isEmpty)
    }

    @Test("image generation sends business material without provider instructions")
    func illustrationRequestContainsOnlyBrief() async throws {
        let id = UUID().uuidString.lowercased()
        let recorder = RequestRecorder()
        let client = Client(serverURL: URL(string: "https://api.journal.example")!, transport: JournalAPITransport { request in
            await recorder.record(request)
            return .init(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"id":"\#(id)","versionID":"\#(id)","state":"queued","revision":1,"expiresAt":"2026-11-01T00:00:00Z"}"#.utf8)))
        })
        _ = try await client.createJournalIllustration(headers: .init(xTellyouwhatRequestID: id),
            body: .json(.init(requestID: id, versionID: id,
                brief: .init(title: "河边", details: [.init(kind: .subject, text: "小狗"), .init(kind: .style, text: "油画")]),
                consentRevision: "2026-09-27")))
        let request = try #require(await recorder.request)
        let body = try #require(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(request.path == "/v1/journal/illustrations")
        #expect(Set(body.keys) == ["requestID", "versionID", "brief", "consentRevision"])
        let brief = try #require(body["brief"] as? [String: Any])
        #expect(brief["title"] as? String == "河边")
        #expect(body["prompt"] == nil && body["instructions"] == nil && body["model"] == nil)
    }

    @Test("scene suggestion has its own route and no book context")
    func illustrationSuggestionRequestIsSeparate() async throws {
        let id = UUID().uuidString.lowercased()
        let hash = String(repeating: "a", count: 64)
        let recorder = RequestRecorder()
        let client = Client(serverURL: URL(string: "https://api.journal.example")!, transport: JournalAPITransport { request in
            await recorder.record(request)
            return .init(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"requestID":"\#(id)","contentHash":"\#(hash)","analysisVersion":"test","quota":{"dailyTokensRemaining":1,"monthlyTokensRemaining":1,"available":true}}"#.utf8)))
        })
        _ = try await client.suggestJournalIllustration(headers: .init(xTellyouwhatRequestID: id),
            body: .json(.init(requestID: id, contentHash: hash, title: "河边", body: "小狗在河边散步。")))
        let request = try #require(await recorder.request)
        let body = try #require(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(request.path == "/v1/journal/illustration-suggestions")
        #expect(Set(body.keys) == ["requestID", "contentHash", "title", "body"])
    }

    @Test("generated activation request has no compatibility body")
    func generatedActivationRequest() async throws {
        let requestID = UUID()
        let recorder = RequestRecorder()
        let transport = JournalAPITransport { request in
            await recorder.record(request)
            return JournalAPIHTTPResponse(
                statusCode: 200,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"status":"active","expiresAt":"2026-09-01T00:00:00Z"}"#.utf8))
            )
        }
        let client = Client(
            serverURL: URL(string: "https://api.journal.example")!,
            transport: transport
        )

        _ = try await client.activateDevelopmentEntitlement(headers: .init(
            xTellyouwhatRequestID: requestID.uuidString.lowercased(),
            xTellyouwhatDevActivation: "development-secret"
        ))

        let request = try #require(await recorder.request)
        #expect(request.operationID == Operations.ActivateDevelopmentEntitlement.id)
        #expect(request.body.isEmpty)
    }
}

private actor RequestRecorder {
    private(set) var requests: [JournalAPIHTTPRequest] = []

    var request: JournalAPIHTTPRequest? {
        requests.last
    }

    func record(_ request: JournalAPIHTTPRequest) {
        requests.append(request)
    }
}
