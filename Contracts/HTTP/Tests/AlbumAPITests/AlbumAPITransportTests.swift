import Foundation
import Testing
@testable import AlbumAPI

@Suite("Album generated control API")
struct AlbumAPITransportTests {
    @Test("subscription response preserves purchase binding separately from media account identity")
    func subscriptionIdentity() async throws {
        let token = "c541c2c4-9a23-5f20-a493-a201b6123d90"
        for stable in [false, true] {
            var payload: [String: Any] = ["accountID": Self.uploadID, "state": "none", "quotaBytes": 0,
                "usedBytes": 0, "reservedBytes": 0, "availableBytes": 0, "revision": 0, "purchasesEnabled": true]
            if stable { payload["appAccountToken"] = token }
            let data = try JSONSerialization.data(withJSONObject: payload)
            let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
                #expect(request.method == "GET" && request.path == "/v1/albums/subscription" && request.body.isEmpty)
                return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")], body: .data(data))
            })
            let response = try await client.getAlbumSubscription()
            guard case .ok(let value) = response else { Issue.record("Unexpected subscription response"); return }
            let account = try value.body.json
            #expect(account.accountID == Self.uploadID)
            #expect(account.appAccountToken == (stable ? token : nil))
        }
    }

    @Test("push registration is session scoped and preserves explicit disable")
    func pushRegistration() async throws {
        let recorder = AlbumRequestRecorder()
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"revision":0,"state":"disabled"}"#.utf8)))
        })
        let initial = try await client.getAlbumPushRegistration()
        switch initial {
        case .ok(let value):
            let state = try value.body.json
            #expect(state.registrationID == nil && state.revision == 0 && state.state.rawValue == "disabled")
        default: Issue.record("Unexpected registration response")
        }
        let read = try #require(await recorder.last)
        #expect(read.method == "GET" && read.path == "/v1/albums/notifications/registration" && read.body.isEmpty)
        _ = try await client.updateAlbumPushRegistration(body: .json(.init(requestID: Self.uploadID, expectedRevision: 7, enabled: false)))
        let write = try #require(await recorder.last)
        #expect(write.method == "PUT" && write.path == read.path)
        let json = try #require(JSONSerialization.jsonObject(with: write.body) as? [String: Any])
        #expect(Set(json.keys) == ["requestID", "expectedRevision", "enabled"])
        #expect(json["enabled"] as? Bool == false && json["expectedRevision"] as? Int == 7)
    }

    @Test("push registration conflict remains a conflict and is not retried")
    func pushRegistrationConflict() async throws {
        let recorder = AlbumRequestRecorder()
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 409, headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"code":"push_registration_changed"}"#.utf8)))
        })
        let response = try await client.updateAlbumPushRegistration(body: .json(.init(requestID: Self.uploadID, expectedRevision: 1, enabled: true, deviceToken: "abcd")))
        switch response {
        case .conflict(let value): #expect(try value.body.json.code == "push_registration_changed")
        default: Issue.record("Conflict was converted to success")
        }
        let request = try #require(await recorder.last)
        let json = try #require(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(json["deviceToken"] as? String == "abcd")
        #expect(json["accountID"] == nil && json["environment"] == nil)
        #expect(await recorder.count == 1)
    }

    @Test("Finished live pair contract keeps QuickTime bytes separate from original and MP4 receipts")
    func finishedLivePair() async throws {
        let recorder = AlbumRequestRecorder(), digest = String(repeating: "a", count: 64)
        let descriptorJSON: [String: Any] = [
            "sourcePhoto": ["preset": "daily_jpeg_v1", "sizeBytes": 123, "sha256": digest, "width": 800, "height": 600, "contentType": "image/jpeg"],
            "sourceVideo": ["preset": "daily_mp4_1080_v1", "sizeBytes": 456, "sha256": digest, "width": 1920, "height": 1080,
                            "durationMilliseconds": 1000, "frameRate": 30, "audioChannels": 2],
            "timing": ["photoResourceID": "photo", "videoResourceID": "motion", "photoTimeMicroseconds": 500001, "durationMicroseconds": 1000001],
            "photo": ["sizeBytes": 140, "sha256": digest], "movie": ["sizeBytes": 500, "sha256": digest],
            "pair": ["identifier": Self.uploadID, "photoTime": 0.500001, "duration": 1.000001, "photoWidth": 800, "photoHeight": 600,
                     "hasAudio": true, "hasHDRVideo": false]
        ]
        let descriptor = try JSONDecoder().decode(Components.Schemas.LivePairDescriptor.self, from: JSONSerialization.data(withJSONObject: descriptorJSON))
        let uploadJSON: [String: Any] = ["id": Self.uploadID, "requestID": Self.uploadID, "assetID": "asset-1", "versionID": Self.uploadID,
            "manifestDigest": digest, "preset": "daily_live_pair_v1", "descriptor": descriptorJSON, "state": "queued", "expiresAt": "2026-10-11T02:00:00Z"]
        let uploadData = try JSONSerialization.data(withJSONObject: uploadJSON)
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 201, headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(uploadData))
        })
        let created = try await client.createAlbumLivePair(path: .init(versionID: Self.uploadID), body: .json(.init(requestID: Self.uploadID, descriptor: descriptor)))
        guard case .created(let value) = created, case .json(let upload) = value.body else { Issue.record("Wrong pair state"); return }
        #expect(upload.state.rawValue == "queued" && upload.descriptor.movie.sizeBytes == 500)
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/library/versions/" + Self.uploadID + "/live-pair")
        let posted = try #require(JSONSerialization.jsonObject(with: request.body) as? [String: Any])
        #expect(posted["requestID"] as? String == Self.uploadID && posted["state"] == nil)
        let photo: [String: Any] = ["objectVersionID": "photo-v1", "sizeBytes": 140, "sha256": digest, "contentType": "image/jpeg",
            "url": "https://storage.example/photo", "headers": [:], "expiresAt": "2026-10-11T02:00:00Z"]
        let movie: [String: Any] = ["objectVersionID": "movie-v1", "sizeBytes": 500, "sha256": digest, "contentType": "video/quicktime",
            "url": "https://storage.example/motion", "headers": [:], "expiresAt": "2026-10-11T02:00:00Z"]
        let readData = try JSONSerialization.data(withJSONObject: ["pairUploadID": Self.uploadID, "assetID": "asset-1", "versionID": Self.uploadID, "manifestDigest": digest,
                    "preset": "daily_live_pair_v1", "descriptor": descriptorJSON, "photo": photo, "movie": movie,
                    "replacementEligible": false, "expiresAt": "2026-10-11T02:00:00Z"])
        let reader = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(readData))
        })
        let read = try await reader.authorizeAlbumLivePairRead(path: .init(versionID: Self.uploadID))
        guard case .ok(let result) = read, case .json(let grant) = result.body else { Issue.record("Wrong pair grant"); return }
        #expect(grant.movie.contentType.rawValue == "video/quicktime" && !grant.replacementEligible)
        #expect(grant.descriptor.timing.photoTimeMicroseconds == 500001)
        let readRequest = try #require(await recorder.last)
        #expect(readRequest.method == "POST" && readRequest.body.isEmpty)
    }
    @Test("Live Photo grant retains same-source components and integer key-photo timing")
    func livePhotoPreviewReadGrant() async throws {
        let recorder = AlbumRequestRecorder(), digest = String(repeating: "a", count: 64)
        let common: [String:Any] = ["assetID": "asset-1", "versionID": Self.uploadID, "manifestDigest": digest,
            "objectVersionID": "immutable-version", "sizeBytes": 1234, "sha256": String(repeating: "b", count: 64),
            "replacementEligible": false, "url": "https://storage.example/read", "headers": [:], "expiresAt": "2026-10-11T02:00:00Z"]
        var photo = common, video = common
        photo["preset"] = "daily_jpeg_v1"; photo["contentType"] = "image/jpeg"; photo["width"] = 800; photo["height"] = 600
        video["preset"] = "daily_mp4_1080_v1"; video["contentType"] = "video/mp4"
        video["profile"] = ["width": 1920, "height": 1080, "durationMilliseconds": 1000, "frameRate": 30, "audioChannels": 2]
        let body = try JSONSerialization.data(withJSONObject: ["assetID": "asset-1", "versionID": Self.uploadID,
            "manifestDigest": digest, "photo": photo, "video": video, "replacementEligible": false,
            "timing": ["photoResourceID": "photo", "videoResourceID": "motion", "photoTimeMicroseconds": 500001, "durationMicroseconds": 1000001],
            "expiresAt": "2026-10-11T02:00:00Z"])
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")], body: .data(body))
        })
        let response = try await client.authorizeAlbumLivePhotoPreviewRead(path: .init(versionID: Self.uploadID))
        guard case .ok(let result) = response, case .json(let grant) = result.body else { Issue.record("Wrong live response"); return }
        #expect(grant.manifestDigest == digest && grant.photo.manifestDigest == digest && grant.video.manifestDigest == digest)
        #expect(grant.timing.photoTimeMicroseconds == 500001 && grant.timing.durationMicroseconds == 1000001)
        #expect(grant.photo.contentType == .imageJpeg && grant.video.contentType == .videoMp4)
        #expect(!grant.replacementEligible && !grant.photo.replacementEligible && !grant.video.replacementEligible)
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/library/versions/" + Self.uploadID + "/live-previews/daily/read-grant")
        #expect(request.method == "POST" && request.body.isEmpty)
    }
    @Test("stored video grant preserves exact media metadata and does not authorize system deletion")
    func videoPreviewReadGrant() async throws {
        let recorder = AlbumRequestRecorder()
        let body = try JSONSerialization.data(withJSONObject: [
            "assetID": "asset-1", "versionID": Self.uploadID, "manifestDigest": String(repeating: "a", count: 64),
            "preset": "daily_mp4_1080_v1", "objectVersionID": "video+/=v1", "sizeBytes": 123456,
            "sha256": String(repeating: "b", count: 64),
            "profile": ["width": 1080, "height": 1920, "durationMilliseconds": 1234, "frameRate": 60, "audioChannels": 2],
            "contentType": "video/mp4", "replacementEligible": false,
            "url": "https://storage.example/signed", "headers": [:], "expiresAt": "2026-10-11T02:00:00Z"
        ])
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")], body: .data(body))
        })
        let preset = try #require(Components.Schemas.VideoPreset(rawValue: "daily_mp4_1080_v1"))
        let response = try await client.authorizeAlbumVideoPreviewRead(path: .init(versionID: Self.uploadID, preset: preset))
        switch response {
        case .ok(let value):
            switch value.body {
            case .json(let grant):
                #expect(grant.objectVersionID == "video+/=v1")
                #expect(grant.sha256 == String(repeating: "b", count: 64))
                #expect(grant.profile.width == 1080 && grant.profile.height == 1920)
                #expect(grant.profile.durationMilliseconds == 1234)
                #expect(grant.profile.frameRate == 60 && grant.profile.audioChannels == 2)
                #expect(grant.replacementEligible == false)
            }
        default: Issue.record("Unexpected video authorization response")
        }
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/library/versions/" + Self.uploadID + "/video-previews/daily_mp4_1080_v1/read-grant")
        #expect(request.method == "POST" && request.body.isEmpty)
    }

    @Test("stored still preview grant preserves verification metadata without replacement authority")
    func photoPreviewReadGrant() async throws {
        let recorder = AlbumRequestRecorder()
        let body = try JSONSerialization.data(withJSONObject: [
            "assetID": "asset-1", "versionID": Self.uploadID, "manifestDigest": String(repeating: "a", count: 64),
            "preset": "daily_jpeg_v1", "objectVersionID": "preview+/=v1", "sizeBytes": 123,
            "sha256": String(repeating: "b", count: 64), "width": 800, "height": 600,
            "contentType": "image/jpeg", "replacementEligible": false,
            "url": "https://storage.example/signed", "headers": [:], "expiresAt": "2026-10-11T02:00:00Z"
        ])
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")], body: .data(body))
        })
        let response = try await client.authorizeAlbumPhotoPreviewRead(path: .init(versionID: Self.uploadID, preset: .dailyJpegV1))
        switch response {
        case .ok(let value):
            switch value.body {
            case .json(let grant):
                #expect(grant.objectVersionID == "preview+/=v1")
                #expect(grant.manifestDigest == String(repeating: "a", count: 64))
                #expect(grant.sha256 == String(repeating: "b", count: 64))
                #expect(grant.sizeBytes == 123)
                #expect(grant.width == 800 && grant.height == 600)
                #expect(grant.replacementEligible == false)
            }
        default: Issue.record("Unexpected preview authorization response")
        }
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/library/versions/" + Self.uploadID + "/photo-previews/daily_jpeg_v1/read-grant")
        #expect(request.method == "POST")
        #expect(request.body.isEmpty)
    }

    @Test("original read authorization uses an account-owned version and no caller object path")
    func originalReadGrant() async throws {
        let recorder = AlbumRequestRecorder()
        let body = try JSONSerialization.data(withJSONObject: [
            "assetID": "asset-1", "versionID": Self.uploadID, "manifestDigest": String(repeating: "a", count: 64),
            "resourceID": "photo", "objectVersionID": "immutable-v1", "sizeBytes": 23,
            "sha256": String(repeating: "b", count: 64), "url": "https://storage.example/signed",
            "headers": [:], "expiresAt": "2026-10-11T02:00:00Z"
        ])
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")], body: .data(body))
        })
        let response = try await client.authorizeAlbumOriginalRead(path: .init(versionID: Self.uploadID, resourceID: "photo"))
        switch response {
        case .ok(let value):
            switch value.body {
            case .json(let grant):
                #expect(grant.versionID == Self.uploadID)
                #expect(grant.objectVersionID == "immutable-v1")
                #expect(grant.sizeBytes == 23)
            }
        default: Issue.record("Unexpected original authorization response")
        }
        let request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/library/versions/" + Self.uploadID + "/resources/photo/read-grant")
        #expect(request.method == "POST")
        #expect(request.body.isEmpty)
    }

    @Test("verified version catalogue keeps owner authority out of queries and preserves cursors")
    func libraryVersions() async throws {
        let recorder = AlbumRequestRecorder()
        let version: [String: Any] = ["sequence": 13, "assetID": "asset-1", "versionID": Self.uploadID,
            "manifestDigest": String(repeating: "a", count: 64),
            "manifest": try JSONSerialization.jsonObject(with: Self.manifestJSON),
            "originalBytes": 23, "publishedAt": "2026-10-11T02:00:00Z"]
        let page = try JSONSerialization.data(withJSONObject: ["versions": [version], "nextSequence": 13, "hasMore": false])
        let single = try JSONSerialization.data(withJSONObject: version)
        let transport = AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(request.operationID == "listAlbumAssetVersions" ? page : single))
        }
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: transport)
        let response = try await client.listAlbumAssetVersions(query: .init(after: 12, limit: 1))
        switch response {
        case .ok(let value):
            switch value.body {
            case .json(let page):
                #expect(page.nextSequence == 13); #expect(page.hasMore == false)
                #expect(page.versions.first?.versionID == Self.uploadID)
            }
        default: Issue.record("Unexpected catalogue response")
        }
        var request = try #require(await recorder.last)
        let url = try #require(URLComponents(string: request.path))
        #expect(url.path == "/v1/albums/library/versions")
        #expect(Dictionary(uniqueKeysWithValues: (url.queryItems ?? []).map { ($0.name, $0.value ?? "") }) == ["after":"12", "limit":"1"])
        #expect(request.method == "GET"); #expect(request.body.isEmpty)
        _ = try await client.getAlbumAssetVersion(path: .init(versionID: Self.uploadID))
        request = try #require(await recorder.last)
        #expect(request.path == "/v1/albums/library/versions/" + Self.uploadID)
        #expect(request.body.isEmpty)
    }

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

    @Test("resource descriptors survive generated client roundtrip; legacy remains absent")
    func reconstructionDescriptors() throws {
        var object = try #require(JSONSerialization.jsonObject(with: Self.manifestJSON) as? [String: Any])
        var resources = try #require(object["resources"] as? [[String: Any]])
        resources[0]["descriptor"] = ["photoKitType": 1, "filename": "原图.heic", "uniformTypeIdentifier": "public.heic"]
        object["resources"] = resources
        object["metadata"] = ["creationTimeMilliseconds": -1, "pixelWidth": 4000, "pixelHeight": 3000, "durationMilliseconds": 0, "mediaSubtypes": 0, "favorite": true, "location": ["latitude": 0, "longitude": 0]]
        let manifest = try JSONDecoder().decode(Components.Schemas.Manifest.self, from: JSONSerialization.data(withJSONObject: object))
        #expect(manifest.resources[0].descriptor?.photoKitType == 1)
        #expect(manifest.resources[0].descriptor?.filename == "原图.heic")
        let decoded = try JSONDecoder().decode(Components.Schemas.Manifest.self, from: JSONEncoder().encode(manifest))
        #expect(decoded.resources[0].descriptor?.uniformTypeIdentifier == "public.heic")
        #expect(decoded.metadata?.creationTimeMilliseconds == -1)
        #expect(decoded.metadata?.favorite == true)
        #expect(decoded.metadata?.location?.longitude == 0)
        #expect(decoded.metadata?.location?.altitude == nil)
        let legacy = try JSONDecoder().decode(Components.Schemas.Manifest.self, from: Self.manifestJSON)
        #expect(legacy.resources[0].descriptor == nil)
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

    @Test("account deletion serializes only a request ID and returns acceptance, not completion")
    func accountDeletion() async throws {
        let recorder = AlbumRequestRecorder()
        let requestID = "9987195d-bcd7-41f2-baa6-6a426cffea59"
        let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
            await recorder.record(request)
            return AlbumAPIHTTPResponse(statusCode: 202,
                headers: [.init(name: "Content-Type", value: "application/json")],
                body: .data(Data(#"{"requestID":"\#(requestID)","requestedAt":"2026-10-13T02:00:00Z","state":"accepted"}"#.utf8)))
        })
        let response = try await client.requestAlbumAccountDeletion(body: .json(.init(requestID: requestID)))
        if case .accepted(let value) = response, case .json(let receipt) = value.body {
            #expect(receipt.requestID == requestID)
            #expect(receipt.state == .accepted)
        } else { Issue.record("Missing durable acceptance") }
        let request = try #require(await recorder.last)
        #expect(request.method == "POST" && request.path == "/v1/albums/account/deletion")
        let body = try #require(JSONSerialization.jsonObject(with: request.body) as? [String: String])
        #expect(body == ["requestID": requestID])
    }

    @Test("withdrawal keeps accepted deletion separate from a successful withdrawal")
    func deletionWithdrawal() async throws {
        for state in ["withdrawn", "accepted"] {
            let recorder = AlbumRequestRecorder()
            let requestID = "9987195d-bcd7-41f2-baa6-6a426cffea59"
            let client = Client(serverURL: URL(string: "https://albums.example")!, transport: AlbumAPITransport { request in
                await recorder.record(request)
                var result = ["requestID": requestID, "state": state]
                if state == "accepted" { result["requestedAt"] = "2026-10-13T02:00:00Z" }
                return AlbumAPIHTTPResponse(statusCode: 200, headers: [.init(name: "Content-Type", value: "application/json")],
                    body: .data(try JSONSerialization.data(withJSONObject: result)))
            })
            let response = try await client.withdrawAlbumAccountDeletion(body: .json(.init(requestID: requestID)))
            if case .ok(let value) = response, case .json(let receipt) = value.body {
                #expect(receipt.requestID == requestID)
                #expect(receipt.state.rawValue == state)
                #expect((receipt.requestedAt != nil) == (state == "accepted"))
            } else { Issue.record("Missing withdrawal outcome") }
            let request = try #require(await recorder.last)
            #expect(request.method == "POST" && request.path == "/v1/albums/account/deletion/withdrawal")
            let body = try #require(JSONSerialization.jsonObject(with: request.body) as? [String: String])
            #expect(body == ["requestID": requestID])
        }
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
    private(set) var count = 0
    func record(_ request: AlbumAPIHTTPRequest) { last = request; count += 1 }
}
