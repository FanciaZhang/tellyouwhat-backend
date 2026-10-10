import Foundation
import Testing
@testable import AlbumAPI

@Suite("Album generated control API")
struct AlbumAPITransportTests {
    @Test("submit is bodyless and never manufactures backup evidence")
    func submitUpload() async throws {
        let recorder = AlbumRequestRecorder()
        let transport = AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 202,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Self.uploadJSON))
        }
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: transport)
        let response = try await client.submitAlbumUpload(path: .init(id: Self.uploadID))
        switch response {
        case .accepted(let value):
            switch value.body {
            case .json(let upload): #expect(upload.state == .queued)
            }
        default: Issue.record("Unexpected submit response")
        }
        let request = try #require(await recorder.last)
        #expect(request.method == "POST")
        #expect(request.path == "/v1/albums/uploads/\(Self.uploadID)/submit")
        #expect(request.body.isEmpty)
        #expect(request.operationID == "submitAlbumUpload")
    }

    @Test("manifest preserves explicit unedited state and rejects missing state")
    func manifestEditState() throws {
        let manifest = try JSONDecoder().decode(Components.Schemas.Manifest.self, from: Self.manifestJSON)
        let encoded = try JSONSerialization.jsonObject(with: JSONEncoder().encode(manifest)) as? [String: Any]
        #expect(encoded?["edited"] as? Bool == false)
        var missing = try #require(encoded)
        missing.removeValue(forKey: "edited")
        let missingJSON = try JSONSerialization.data(withJSONObject: missing)
        #expect(throws: DecodingError.self) {
            _ = try JSONDecoder().decode(Components.Schemas.Manifest.self, from: missingJSON)
        }
    }

    @Test("generated grant path carries no owner or client proof")
    func uploadGrant() async throws {
        let recorder = AlbumRequestRecorder()
        let transport = AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"grants":[{"resourceID":"photo","url":"https://storage.example/staging","headers":{"Content-Length":"23"},"expiresAt":"2026-10-11T02:00:00Z"}]}"#.utf8)))
        }
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: transport)
        _ = try await client.authorizeAlbumUploadResources(path: .init(id: Self.uploadID))
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/uploads/\(Self.uploadID)/grants")
        #expect(request.body.isEmpty)
    }

    @Test("create serializes manifest without client authority fields")
    func createUpload() async throws {
        let recorder = AlbumRequestRecorder()
        let transport = AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 201,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Self.uploadJSON))
        }
        let manifest = try JSONDecoder().decode(Components.Schemas.Manifest.self, from: Self.manifestJSON)
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: transport)
        _ = try await client.createAlbumUpload(body: .json(.init(requestID: Self.uploadID, manifest: manifest)))
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/uploads")
        #expect(request.method == "POST")
        let body = try #require(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(Set(body.keys) == Set(["requestID", "manifest"]))
        let wireManifest = try #require(body["manifest"] as? [String: Any])
        #expect(wireManifest["edited"] as? Bool == false)
    }

    @Test("login serializes proof without client account authority")
    func appleLogin() async throws {
        let recorder = AlbumRequestRecorder()
        let transport = AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"token":"opaque-test-token","expiresAt":"2026-11-11T02:00:00Z","accountID":"324aa069-b7da-4d23-88e7-4c8d54bd4749"}"#.utf8)))
        }
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: transport)
        let response = try await client.loginAlbumWithApple(body: .json(.init(
            challengeID: Self.uploadID, proof: String(repeating: "a", count: 43),
            authorizationCode: "test-code", identityToken: "test-assertion")))
        switch response {
        case .ok(let value):
            switch value.body {
            case .json(let session):
                #expect(session.accountID == Self.uploadID)
                #expect(session.token == "opaque-test-token")
            }
        default: Issue.record("Unexpected login response")
        }
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/auth/apple")
        #expect(request.method == "POST")
        let body = try #require(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(Set(body.keys) == Set(["challengeID", "proof", "authorizationCode", "identityToken"]))
    }

    @Test("logout is bodyless and accepts an empty response")
    func logout() async throws {
        let recorder = AlbumRequestRecorder()
        let transport = AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 204, headers: [], body: .data(Data()))
        }
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: transport)
        let response = try await client.logoutAlbumSession()
        switch response {
        case .noContent: break
        default: Issue.record("Unexpected logout response")
        }
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/auth/logout")
        #expect(request.body.isEmpty)
    }

    @Test("multipart control preserves byte ranges and bodyless authority")
    func multipartControl() async throws {
        let recorder = AlbumRequestRecorder()
        let transport = AlbumAPITransport { request in
            await recorder.record(request)
            let json: String
            switch request.operationID {
            case "initializeAlbumMultipart":
                json = #"{"resourceID":"video","sizeBytes":8388625,"partSize":8388608,"partCount":2}"#
            case "authorizeAlbumMultipartPart":
                json = #"{"resourceID":"video","url":"https://storage.example/part","headers":{"Content-Length":"17"},"expiresAt":"2026-10-11T02:00:00Z","partNumber":2,"offset":8388608,"sizeBytes":17}"#
            case "getAlbumMultipart":
                json = #"{"resourceID":"video","state":"pending","parts":[]}"#
            case "completeAlbumMultipart":
                json = #"{"resourceID":"video","state":"assembled"}"#
            default: throw URLError(.badURL)
            }
            return AlbumAPIHTTPResponse(statusCode: 200,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(json.utf8)))
        }
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: transport)
        let initialized = try await client.initializeAlbumMultipart(path: .init(id: Self.uploadID, resourceID: "video"))
        if case .ok(let value) = initialized, case .json(let session) = value.body {
            #expect(session.partCount == 2)
        } else { Issue.record("Missing multipart session") }
        var request = try #require(await recorder.last)
        #expect(request.method == "POST" && request.body.isEmpty)
        #expect(request.path == "/v1/albums/uploads/\(Self.uploadID)/resources/video/multipart")
        let grant = try await client.authorizeAlbumMultipartPart(path: .init(id: Self.uploadID, resourceID: "video", partNumber: 2))
        if case .ok(let value) = grant, case .json(let part) = value.body {
            #expect(part.offset == 8388608 && part.sizeBytes == 17)
        } else { Issue.record("Missing exact part range") }
        request = try #require(await recorder.last)
        #expect(request.path.hasSuffix("/parts/2/grant") && request.body.isEmpty)
        let status = try await client.getAlbumMultipart(path: .init(id: Self.uploadID, resourceID: "video"))
        if case .ok(let value) = status, case .json(let progress) = value.body {
            #expect(progress.state == .pending && progress.parts.isEmpty)
        } else { Issue.record("Missing persisted pending state") }
        request = try #require(await recorder.last)
        #expect(request.method == "GET" && request.body.isEmpty)
        let completed = try await client.completeAlbumMultipart(path: .init(id: Self.uploadID, resourceID: "video"))
        if case .ok(let value) = completed, case .json(let result) = value.body {
            #expect(result.state == .assembled)
        } else { Issue.record("Missing assembly result") }
        request = try #require(await recorder.last)
        #expect(request.path.hasSuffix("/complete") && request.method == "POST" && request.body.isEmpty)
    }

    static let uploadID = "324aa069-b7da-4d23-88e7-4c8d54bd4749"
    static let manifestJSON = Data(#"{"assetID":"asset","sourceRevision":"revision-1","kind":"photo","edited":false,"resources":[{"id":"photo","role":"original_photo","sizeBytes":23,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}"#.utf8)
    static let uploadJSON = Data(#"{"id":"324aa069-b7da-4d23-88e7-4c8d54bd4749","requestID":"9987195d-bcd7-41f2-baa6-6a426cffea59","manifest":\#(String(decoding: manifestJSON, as: UTF8.self)),"manifestDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"queued","originalBytes":23,"derivedBudget":23,"expiresAt":"2026-10-11T02:00:00Z"}"#.utf8)
}

private actor AlbumRequestRecorder {
    private(set) var last: AlbumAPIHTTPRequest?
    func record(_ request: AlbumAPIHTTPRequest) { last = request }
}
