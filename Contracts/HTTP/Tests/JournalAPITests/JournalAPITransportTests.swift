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
        #expect(request.path == "/v1/ai/operations/journal.organize/responses")
        #expect(request.header(named: "X-Tellyouwhat-Request-ID") == requestID.uuidString.lowercased())
        #expect(!request.body.isEmpty)
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
