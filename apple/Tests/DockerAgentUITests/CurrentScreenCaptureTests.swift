import XCTest

@MainActor
final class CurrentScreenCaptureTests: XCTestCase {
    private let approvedRootTitle = "iphone/ipad/macos app overhaul"

    func testOpenOnlyApprovedRootForLoadDiagnosis() {
        continueAfterFailure = false
        let app = XCUIApplication(bundleIdentifier: "dev.dockeragent.apple")
        let home = app.buttons["Active server, mbp"]
        if !home.exists {
            let back = app.navigationBars.buttons["BackButton"]
            guard back.exists && back.isHittable else {
                XCTFail("No safe local Back navigation to mbp home")
                return
            }
            back.tap()
        }
        guard home.waitForExistence(timeout: 10) else {
            XCTFail("Only open the approved root from mbp home")
            return
        }
        let root = app.cells.containing(.staticText, identifier: approvedRootTitle).firstMatch
        guard root.waitForExistence(timeout: 10) && root.isHittable else {
            XCTFail("Approved root row unavailable")
            return
        }
        root.tap()
        let rootID = "session.detail.0EE9B6DD-8838-4FE1-961D-8D1017EF0551.5f997513-8797-440b-b3ab-86f74e7bc4c5"
        let exactRoot = app.descendants(matching: .any)[rootID]
        _ = waitForCondition(timeout: 45, {
            app.staticTexts["Couldn't Open Session"].exists || (exactRoot.exists && app.scrollViews.firstMatch.exists)
        })
        addKeptAttachment("approvedRootContentIDPresent=\(exactRoot.exists); loadErrorVisible=\(app.staticTexts["Couldn't Open Session"].exists); scrollPresent=\(app.scrollViews.firstMatch.exists)")
        attachScreenshotOnly(named: "exact approved root actual load diagnostic state")
    }

    func testReturnFromAuthorizedRootLoadErrorToMbpOnly() {
        continueAfterFailure = false
        let app = XCUIApplication(bundleIdentifier: "dev.dockeragent.apple")
        let bar = app.navigationBars[approvedRootTitle]
        guard bar.waitForExistence(timeout: 5), app.staticTexts["Couldn't Open Session"].exists else {
            XCTFail("Only leave the observed authorized root load error")
            return
        }
        let back = bar.buttons["BackButton"]
        guard back.exists && back.isHittable else {
            XCTFail("Authorized root Back control is not safely hittable")
            return
        }
        back.tap()
        XCTAssertTrue(app.buttons["Active server, mbp"].waitForExistence(timeout: 10))
        attachScreenshotOnly(named: "responsive mbp home after leaving bounded load error")
    }

    func testDismissPendingAttachAndInspectorOnly() {
        continueAfterFailure = false
        let app = XCUIApplication(bundleIdentifier: "dev.dockeragent.apple")
        let rootID = "session.detail.0EE9B6DD-8838-4FE1-961D-8D1017EF0551.5f997513-8797-440b-b3ab-86f74e7bc4c5"
        // Verified screenshot: inert Agents heading outside the pending confirmation.
        app.coordinate(withNormalizedOffset: CGVector(dx: 37.0 / 375.0, dy: 130.0 / 812.0)).tap()
        let pendingTitle = app.staticTexts["Open dev-team?"]
        guard waitForCondition(timeout: 5, { !pendingTitle.exists }) else {
            attachScreenshotOnly(named: "cleanup stopped with confirmation still present")
            XCTFail("Pending confirmation did not dismiss; no further action taken")
            return
        }
        let done = app.buttons["inspector.done"]
        guard done.waitForExistence(timeout: 5) && done.isHittable else {
            attachScreenshotOnly(named: "cleanup stopped before unavailable inspector Done")
            XCTFail("Inspector Done is not safely hittable")
            return
        }
        done.tap()
        guard waitForCondition(timeout: 10, {
            app.descendants(matching: .any)[rootID].exists && !done.exists && app.scrollViews.firstMatch.exists
        }) else {
            attachScreenshotOnly(named: "cleanup root verification unavailable")
            XCTFail("Root content was not exposed after safe dismissal")
            return
        }
        addKeptAttachment("verified restored root content identity=" + rootID)
        attachScreenshotOnly(named: "restored responsive authorized root after cancel and Done")
    }

    func testReturnFromVerifiedChildToItsApprovedParent() {
        let app = XCUIApplication(bundleIdentifier: "dev.dockeragent.apple")
        let childNavigation = app.navigationBars["Designer-Led Native Apple App Overhaul"]
        XCTAssertTrue(childNavigation.waitForExistence(timeout: 5), "Recover the still-open existing descendant without reopening either session")
        let childIdentity = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH %@", "session.detail.")).allElementsBoundByIndex.map(\.identifier)
        addKeptAttachment("current descendant content identity identifiers=\(childIdentity.joined(separator: ","))")
        attachScreenshotOnly(named: "existing descendant before safe parent return")

        let expectedParentAction = app.buttons["Open parent session \(approvedRootTitle)"]
        XCTAssertTrue(expectedParentAction.waitForExistence(timeout: 5) && expectedParentAction.isHittable, "Use the child view's explicitly scoped Open parent action for the approved root")
        let actionLabel = expectedParentAction.label
        addKeptAttachment("verified parent navigation action=\(actionLabel)")
        expectedParentAction.tap()

        let rootNavigation = app.navigationBars[approvedRootTitle]
        XCTAssertTrue(rootNavigation.waitForExistence(timeout: 20), "Return through the child session's parent SessionRef to the approved root")
        let rootIdentity = app.descendants(matching: .any).matching(NSPredicate(format: "identifier BEGINSWITH %@", "session.detail.")).allElementsBoundByIndex.map(\.identifier)
        addKeptAttachment("returned root content identity identifiers=\(rootIdentity.joined(separator: ","))")
        attachScreenshotOnly(named: "returned to approved root via child parent action")
    }

    func testAttachExistingCurrentRootDescendantByStableRefAndReturnToRoot() {
        continueAfterFailure = false
        let app = XCUIApplication(bundleIdentifier: "dev.dockeragent.apple")
        let serverID = "0EE9B6DD-8838-4FE1-961D-8D1017EF0551"
        let rootSessionID = "5f997513-8797-440b-b3ab-86f74e7bc4c5"
        let rootContentID = "session.detail.\(serverID).\(rootSessionID)"
        let rootContent = app.descendants(matching: .any)[rootContentID]

        if !rootContent.exists {
            guard app.buttons["Active server, mbp"].waitForExistence(timeout: 10) else {
                XCTFail("Start only from the known authorized mbp home or the already-open approved root")
                return
            }
            let rootRow = app.cells.containing(.staticText, identifier: approvedRootTitle).firstMatch
            guard rootRow.waitForExistence(timeout: 10) && rootRow.isHittable else {
                XCTFail("Address only the approved root row")
                return
            }
            rootRow.tap()
        }
        guard waitForCondition(timeout: 25, {
            rootContent.exists && app.scrollViews.firstMatch.exists && !app.staticTexts["Couldn't Open Session"].exists
        }) else {
            XCTFail("Wait for confirmed content under the exact approved root SessionRef")
            return
        }
        attachScreenshotOnly(named: "approved root content stable identity before child attach")

        let inspector = app.buttons["Inspector"]
        guard inspector.waitForExistence(timeout: 10) && inspector.isEnabled && inspector.isHittable else {
            XCTFail("Inspector is unavailable on the loaded authorized root")
            return
        }
        inspector.tap()
        attachScreenshotOnly(named: "approved root inspector existing child list")

        // These refs were read from the authorized root's structured snapshot tree, not parsed from AX.
        let nodeID = "8958a"
        let childSessionID = "98a93627-f4f5-4192-9ce1-62cb8049d29f"
        let target = "\(serverID).\(childSessionID)"
        let attachID = "teamlist.node.\(nodeID).session.\(target)"
        let inspectID = "teamlist.inspect.\(nodeID)"
        let childContentID = "session.detail.\(target)"
        let confirmationID = "team.attach.confirm.\(nodeID)"
        let listToggle = app.buttons["List"]
        if listToggle.exists && listToggle.isHittable { listToggle.tap() }
        let childRow = app.buttons[attachID]
        let inspect = app.buttons[inspectID]
        guard waitForCondition(timeout: 5, { childRow.exists && inspect.exists }),
              childRow.isEnabled && childRow.isHittable && inspect.isEnabled && inspect.isHittable else {
            attachScreenshotOnly(named: "discrete child attach and Inspect controls unavailable")
            XCTFail("Expected two discrete model-bound controls for the known descendant")
            return
        }
        XCTAssertEqual(childRow.identifier, attachID)
        XCTAssertEqual(inspect.identifier, inspectID)
        XCTAssertNotEqual(childRow.frame, inspect.frame, "Attach and Inspect must not share a merged List-row target")
        let merged = app.buttons.matching(NSPredicate(format: "identifier CONTAINS %@ AND identifier CONTAINS %@", attachID, inspectID))
        XCTAssertEqual(merged.count, 0, "Never authorize attachment through a concatenated accessibility identifier")
        addKeptAttachment("model verified node=\(nodeID) target=\(target); discrete attach=\(childRow.identifier); inspect=\(inspect.identifier)")
        attachScreenshotOnly(named: "known descendant discrete Attach and Inspect controls")
        childRow.tap()

        let targetConfirmation = app.buttons[confirmationID]
        let genericConfirmation = app.buttons["Open Session"]
        guard waitForCondition(timeout: 10, {
            (targetConfirmation.exists && targetConfirmation.isHittable) || (genericConfirmation.exists && genericConfirmation.isHittable)
        }) else {
            XCTFail("Server identity/ancestry confirmation did not appear for the selected child")
            return
        }
        let exactConfirmation = targetConfirmation.exists && targetConfirmation.isHittable ? targetConfirmation : genericConfirmation
        let confirmationTarget = app.descendants(matching: .any)["team.attach.target.\(serverID).\(childSessionID)"]
        if confirmationTarget.exists {
            XCTAssertEqual(confirmationTarget.identifier, "team.attach.target.\(serverID).\(childSessionID)")
            addKeptAttachment("server confirmation target identifier matched selected SessionRef")
        } else {
            addKeptAttachment("Confirmation message AX identifier was dropped; source button independently established exact ref=\(target)")
        }
        attachScreenshotOnly(named: "verified exact child target confirmation")
        exactConfirmation.tap()

        guard waitForCondition(timeout: 5, {
            let done = app.buttons["inspector.done"]
            return !done.exists || !done.isHittable
        }) else {
            XCTFail("The native child-attach action did not dismiss the inspector")
            return
        }
        let childContent = app.descendants(matching: .any)[childContentID]
        guard waitForCondition(timeout: 20, {
            childContent.exists && app.scrollViews.firstMatch.exists && !app.staticTexts["Couldn't Open Session"].exists
        }) else {
            XCTFail("The attached content did not match the exact selected child SessionRef")
            return
        }
        addKeptAttachment("verified child content identity=\(childContent.identifier)")
        attachScreenshotOnly(named: "verified existing descendant content")

        let back = app.navigationBars.buttons["BackButton"]
        guard back.exists && back.isHittable else {
            XCTFail("The verified child has no hittable NavigationStack back action")
            return
        }
        back.tap()
        guard waitForCondition(timeout: 20, {
            rootContent.exists && app.scrollViews.firstMatch.exists && !app.staticTexts["Couldn't Open Session"].exists
        }) else {
            XCTFail("Back did not return to the exact root content SessionRef")
            return
        }
        attachScreenshotOnly(named: "verified return to authorized root content identity")
    }

    func testAuthorizedCurrentSessionControlsAndLiveScroll() {
        let app = XCUIApplication(bundleIdentifier: "dev.dockeragent.apple")
        let navigationBar = app.navigationBars.firstMatch
        XCTAssertTrue(navigationBar.waitForExistence(timeout: 5), "Inspect the currently running app without launching it")
        let navigationID = navigationBar.identifier
        add(XCTAttachment(string: "foreground navigation identifier=\(navigationID) label=\(navigationBar.label)"))
        if navigationID != approvedRootTitle {
            let home = app.buttons["Active server, mbp"]
            guard home.exists else {
                XCTFail("Expected either the exact approved root or the existing mbp server home")
                return
            }
            let rootCell = app.cells.containing(.staticText, identifier: approvedRootTitle).firstMatch
            guard rootCell.waitForExistence(timeout: 10) && rootCell.isHittable else {
                XCTFail("The exact approved root must be present on the mbp home before opening it")
                return
            }
            rootCell.tap()
            XCTAssertTrue(app.navigationBars[approvedRootTitle].waitForExistence(timeout: 20), "Open the exact authorized root once from the mbp home")
        }
        let authorizedNavigationBar = app.navigationBars[approvedRootTitle]
        XCTAssertTrue(authorizedNavigationBar.exists)
        attachScreenshotOnly(named: "authorized root before runtime audit")

        let done = app.buttons["inspector.done"]
        if done.exists {
            guard done.isHittable else {
                XCTFail("Inspector Done exists but is not safely hittable")
                return
            }
            done.tap()
            XCTAssertFalse(done.exists, "The inspector was dismissed using its explicit Done control")
        }

        let transcript = app.scrollViews.firstMatch
        XCTAssertTrue(transcript.waitForExistence(timeout: 5))
        recordVisibleTranscriptGeometry(app, transcript, stage: "initial foreground")
        sleep(4)
        recordVisibleTranscriptGeometry(app, transcript, stage: "initial foreground after passive wait")

        // Inspect only the currently loaded visible history; do not synthesize work to create fixture rows.
        let visibleToolFamilies = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "tool-row-header-"))
            .allElementsBoundByIndex.map { $0.identifier }
        addKeptAttachment("currently visible tool family identifiers: \(visibleToolFamilies.joined(separator: ","))")
        attachScreenshotOnly(named: "runtime currently visible transcript controls")


        let inspector = app.buttons["Inspector"]
        XCTAssertTrue(inspector.waitForExistence(timeout: 5) && inspector.isHittable, "The approved session remains active before opening its inspector")
        inspector.tap()
        attachScreenshotOnly(named: "runtime inspector agents list")
        let listSegment = app.buttons.matching(NSPredicate(format: "label == %@", "List")).firstMatch
        if listSegment.exists && listSegment.isHittable { listSegment.tap() }
        attachScreenshotOnly(named: "runtime inspector agents list for child attachment")

        let directChild = app.buttons.matching(NSPredicate(format: "label CONTAINS %@", "dev-team"))
            .allElementsBoundByIndex.filter { $0.isHittable && $0.label.contains("dev-team") }
        let childLabels = directChild.map(\.label)
        addKeptAttachment("visible child-row candidates: \(childLabels.joined(separator: " | "))")
        let workingChildRows = directChild.filter { $0.label.contains("Working") && $0.label.contains("Running") }
        var childAttached = false
        if let childRow = workingChildRows.first {
            let label = childRow.label
            addKeptAttachment("Selecting an actual List-bound live descendant row (\(workingChildRows.count) same-name rows visible); the bound node SessionRef and server's existing confirmation/ancestry checks determine the session.")
            childRow.tap()
            let openButton = app.buttons["Open Session"]
            if openButton.waitForExistence(timeout: 5) {
                let confirmationTexts = app.staticTexts.allElementsBoundByIndex.map(\.label)
                let guardText = confirmationTexts.first(where: { $0.localizedCaseInsensitiveContains("confirms its identity and team ancestry") })
                let expectedAgent = confirmationTexts.contains(where: { $0 == "Open dev-team?" })
                addKeptAttachment("selected list child row=\(label); confirmation names expected agent=\(expectedAgent); identity-and-ancestry-guard=\(guardText != nil)")
                attachScreenshotOnly(named: "runtime verified child identity confirmation")
                if expectedAgent, guardText != nil, openButton.isHittable {
                    openButton.tap()
                    let childNav = app.navigationBars.firstMatch
                    XCTAssertTrue(childNav.waitForExistence(timeout: 15), "The selected existing child session should attach after its identity guard")
                    let childTitle = childNav.identifier
                    let childLabel = childNav.label
                    let childAgentPresent = app.staticTexts["dev-team"].exists || childLabel.localizedCaseInsensitiveContains("dev-team")
                    let expectedParent = app.buttons["Open parent session \(approvedRootTitle)"]
                    let hasAuthorizedParent = expectedParent.exists
                    addKeptAttachment("attached descendant nav identifier=\(childTitle) label=\(childLabel) dev-team-visible=\(childAgentPresent) has-parent-button-to-authorized-root=\(hasAuthorizedParent)")
                    attachScreenshotOnly(named: "runtime attached existing dev-team child")
                    XCTAssertTrue(childAgentPresent && hasAuthorizedParent, "The attached session should identify dev-team and point back to the authorized root")
                    childAttached = childAgentPresent && hasAuthorizedParent
                    let back = childNav.buttons["BackButton"]
                    if back.exists && back.isHittable { back.tap() }
                    XCTAssertTrue(app.navigationBars[approvedRootTitle].waitForExistence(timeout: 10), "Return from the attached child to the approved root")
                } else {
                    let cancel = app.buttons["Cancel"]
                    if cancel.exists && cancel.isHittable { cancel.tap() }
                }
            } else {
                addKeptAttachment("The visible dev-team list row did not offer the existing Open Session guard; no session was opened.")
            }
        } else {
            addKeptAttachment("No unique visible attachable dev-team list row was available.")
        }
        if !childAttached {
            let cancel = app.buttons["Cancel"]
            if cancel.exists && cancel.isHittable { cancel.tap() }
        }

        if done.exists && done.isHittable { done.tap() }
        XCTAssertTrue(navigationBar.exists && navigationBar.identifier == approvedRootTitle, "Return from inspector sheet to the same approved root using Done")
        let jump = app.buttons["Jump to Latest"]
        if jump.exists && jump.isEnabled && jump.isHittable {
            jump.tap()
            XCTAssertTrue(waitForCondition(timeout: 10) { !jump.isEnabled && !jump.isHittable }, "Jump should settle at the latest transcript position")
        }
        attachScreenshotOnly(named: "runtime returned to authorized root latest")
        recordVisibleTranscriptGeometry(app, transcript, stage: "latest after audit")
        sleep(5)
        recordVisibleTranscriptGeometry(app, transcript, stage: "latest after passive growth observation")
    }

    private func hittableToolHeader(_ app: XCUIApplication, family: String) -> XCUIElement? {
        let identifier = "tool-row-header-\(family)"
        return app.buttons.matching(NSPredicate(format: "identifier == %@", identifier))
            .allElementsBoundByIndex.first(where: { $0.isHittable })
    }

    private func recordVisibleTranscriptGeometry(_ app: XCUIApplication, _ transcript: XCUIElement, stage: String) {
        let scrollFrame = transcript.frame
        let visibleTexts = transcript.staticTexts.allElementsBoundByIndex
            .filter { scrollFrame.intersects($0.frame) }
            .sorted { $0.frame.maxY < $1.frame.maxY }
        let lastFrame = visibleTexts.last?.frame ?? .zero
        let scrollbar = app.otherElements.matching(NSPredicate(format: "label BEGINSWITH %@", "Vertical scroll bar")).firstMatch
        let jump = app.buttons["Jump to Latest"]
        let percent = scrollbar.value as? String ?? "unavailable"
        addKeptAttachment(String(format: "stage=%@ time=%@ transcript=(%.1f,%.1f %.1fx%.1f) lastVisibleTextFrame=(%.1f,%.1f %.1fx%.1f) visibleTextCount=%d scrollbar=%@ jumpEnabled=%@", stage, ISO8601DateFormatter().string(from: Date()), scrollFrame.minX, scrollFrame.minY, scrollFrame.width, scrollFrame.height, lastFrame.minX, lastFrame.minY, lastFrame.width, lastFrame.height, visibleTexts.count, percent, jump.exists && jump.isEnabled ? "yes" : "no"))
        attachScreenshotOnly(named: "runtime geometry \(stage)")
    }

    func testCaptureCurrentJumpGeometryWithoutOpeningAnotherSession() {
        let app = XCUIApplication(bundleIdentifier: "dev.dockeragent.apple")
        // Query the current foreground state; do not activate or open any session.
        let title = app.navigationBars[approvedRootTitle]
        XCTAssertTrue(title.waitForExistence(timeout: 5))
        let transcript = app.scrollViews.firstMatch
        XCTAssertTrue(transcript.waitForExistence(timeout: 5))

        let done = app.buttons["inspector.done"]
        if done.exists && done.isHittable { done.tap() }

        let latest = app.descendants(matching: .any)["transcript-latest-row"]
        let jump = app.buttons["Jump to Latest"]
        let scrollbar = app.otherElements.matching(NSPredicate(format: "label BEGINSWITH %@", "Vertical scroll bar")).firstMatch
        func bounds(_ element: XCUIElement) -> String {
            let frame = element.frame
            return String(format: "x=%.1f y=%.1f w=%.1f h=%.1f maxY=%.1f", frame.minX, frame.minY, frame.width, frame.height, frame.maxY)
        }
        func record(_ stage: String) {
            let percent = scrollbar.value as? String ?? "unavailable"
            let anchor = latest.exists ? bounds(latest) : "missing"
            add(XCTAttachment(string: "stage=\(stage) scroll=\(bounds(transcript)) visible=\(bounds(transcript)) latest=\(anchor) jumpEnabled=\(jump.exists && jump.isEnabled) jumpHittable=\(jump.exists && jump.isHittable) jumpFrame=\(jump.exists ? bounds(jump) : "missing") scrollbar=\(percent)"))
            attachScreenshotOnly(named: "jump geometry \(stage)")
        }

        record("before manual escape")
        if !jump.isEnabled || !jump.isHittable {
            transcript.swipeDown()
            XCTAssertTrue(waitForCondition(timeout: 5) { jump.isEnabled && jump.isHittable }, "Finger-down scroll should reveal older transcript")
        }
        record("scrolled away from latest")
        XCTAssertTrue(jump.isEnabled && jump.isHittable)
        let beforeAnchor = bounds(latest)
        let beforeScrollbar = scrollbar.value as? String
        jump.tap()
        sleep(2)
        record("after Jump settle")
        let afterAnchor = bounds(latest)
        let afterScrollbar = scrollbar.value as? String
        XCTAssertNotEqual(afterAnchor, beforeAnchor, "The registered last-row anchor should move into the viewport")
        if let beforeScrollbar, let afterScrollbar, let before = Int(beforeScrollbar.replacingOccurrences(of: "%", with: "")), let after = Int(afterScrollbar.replacingOccurrences(of: "%", with: "")) {
            XCTAssertGreaterThan(after, before, "The actual scroll range should advance to latest")
        }
    }

    func testVisualRefinementTargetsStableToolAndMapControls() {
        let app = XCUIApplication(bundleIdentifier: "dev.dockeragent.apple")
        let home = app.buttons["Active server, mbp"]
        if home.waitForExistence(timeout: 5) {
            attachScreenshotOnly(named: "visual refinement mbp home on launch")
            let root = app.cells.containing(.staticText, identifier: approvedRootTitle).firstMatch
            XCTAssertTrue(root.waitForExistence(timeout: 10))
            root.tap()
        }

        let title = app.navigationBars[approvedRootTitle]
        XCTAssertTrue(title.waitForExistence(timeout: 20))
        let transcript = app.scrollViews.firstMatch
        XCTAssertTrue(waitForTranscriptContent(app, timeout: 45), "Do not inspect controls before the exact root has real content")
        XCTAssertTrue(transcript.exists)
        attachScreenshotOnly(named: "visual refinement loaded authorized portrait")

        XCUIDevice.shared.orientation = .landscapeRight
        XCTAssertTrue(transcript.waitForExistence(timeout: 10))
        attachScreenshotOnly(named: "visual refinement loaded authorized landscape")
        XCUIDevice.shared.orientation = .portrait

        let firstTool = app.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "tool-row-header-")).firstMatch
        let scrollBar = app.otherElements.matching(NSPredicate(format: "label BEGINSWITH %@", "Vertical scroll bar")).firstMatch
        var scrolls = 0
        while (!firstTool.exists || !firstTool.isHittable) && scrolls < 12 {
            transcript.swipeDown()
            scrolls += 1
        }
        XCTAssertTrue(firstTool.exists && firstTool.isHittable, "Scroll the loaded transcript until an actual stable-ID tool header is on screen before tapping")
        attachScreenshotOnly(named: "visual refinement collapsed stable-ID tool row")

        let family = firstTool.identifier.replacingOccurrences(of: "tool-row-header-", with: "")
        if family == "command" {
            firstTool.tap()
            XCTAssertTrue(app.otherElements["tool-row-body"].waitForExistence(timeout: 5))
            attachScreenshotOnly(named: "visual refinement expanded command body")
            firstTool.press(forDuration: 1)
            attachScreenshotOnly(named: "visual refinement command safe context")
            // Context content is read-only; dismiss by tapping the inert session title, never a menu action.
            title.staticTexts.firstMatch.tap()
            XCTAssertFalse(app.buttons["Copy Command"].exists)
        } else {
            firstTool.tap()
            XCTAssertTrue(app.otherElements["tool-row-body"].waitForExistence(timeout: 5))
            attachScreenshotOnly(named: "visual refinement expanded noncommand body")
        }

        // Read the scrollbar value only; it is metadata, not transcript content.
        let beforeJumpScrollPercent = scrollBar.value as? String
        if let beforeJumpScrollPercent { add(XCTAttachment(string: "Transcript scrollbar before Jump: \(beforeJumpScrollPercent)")) }
        let jump = app.buttons["Jump to Latest"]
        XCTAssertTrue(jump.isEnabled && jump.isHittable, "Jump is tested only once a loaded transcript and real scroll-away gesture enabled it")
        attachScreenshotOnly(named: "visual refinement Jump enabled after scroll up")
        jump.tap()
        XCTAssertTrue(waitForCondition(timeout: 10) { !jump.isEnabled && !jump.isHittable }, "At latest, Jump must be disabled and out of the hit-test path")
        attachScreenshotOnly(named: "visual refinement Jump settled at latest")
        let latestScrollBar = app.otherElements.matching(NSPredicate(format: "label BEGINSWITH %@", "Vertical scroll bar")).firstMatch
        let afterJumpScrollPercent = latestScrollBar.value as? String
        if let afterJumpScrollPercent { add(XCTAttachment(string: "Transcript scrollbar at latest: \(afterJumpScrollPercent)")) }
        if let beforeJumpScrollPercent, let afterJumpScrollPercent,
           let before = Int(beforeJumpScrollPercent.replacingOccurrences(of: "%", with: "")),
           let after = Int(afterJumpScrollPercent.replacingOccurrences(of: "%", with: "")) {
            XCTAssertGreaterThan(after, before, "Jump moves the visible range toward the latest transcript content")
        }

        // Manual escape from latest must restore the older-row affordance.
        transcript.swipeDown()
        XCTAssertTrue(waitForCondition(timeout: 10) { jump.isEnabled && jump.isHittable }, "Manual content scroll can leave the bottom-follow state")
        attachScreenshotOnly(named: "visual refinement manual scroll escape")

        let inspector = app.buttons["Inspector"]
        XCTAssertTrue(inspector.isEnabled && inspector.isHittable)
        inspector.tap()
        attachScreenshotOnly(named: "visual refinement inspector agents list")
        let agentsSegment = app.buttons.matching(NSPredicate(format: "label == %@", "Agents")).firstMatch
        if agentsSegment.exists && agentsSegment.isHittable { agentsSegment.tap() }
        let mapSegment = app.buttons.matching(NSPredicate(format: "label == %@", "Map")).firstMatch
        XCTAssertTrue(mapSegment.waitForExistence(timeout: 5), "Target the explicit Map segment, not a generic person.2 icon")
        XCTAssertTrue(mapSegment.isHittable)
        mapSegment.tap()
        attachScreenshotOnly(named: "visual refinement team map")

        let map = app.otherElements["Team map"]
        if map.exists {
            let nodes = map.buttons.allElementsBoundByIndex
            XCTAssertFalse(nodes.isEmpty, "Team map provides accessible node children")
            let current = nodes.first { ($0.label as? String)?.contains("This session") == true }
            XCTAssertNotNil(current, "Map marks only the session-specific current agent")
            if let current {
                current.tap()
                attachScreenshotOnly(named: "visual refinement selected current map node")
                current.press(forDuration: 1)
                attachScreenshotOnly(named: "visual refinement map safe context")
                let inertMapHeader = app.staticTexts["Team map"]
                if inertMapHeader.exists { inertMapHeader.tap() }
            }
        } else {
            add(XCTAttachment(string: "Team map view exists, but canvas AX label is not separately queried; visible map captured."))
        }

        let done = app.buttons["inspector.done"]
        XCTAssertTrue(done.waitForExistence(timeout: 5) && done.isHittable, "Dismiss the inspector through its explicit Done control before navigating back")
        done.tap()
        XCTAssertFalse(done.exists)
        backToMbpHome(app, title: title)
    }

    private func backToMbpHome(_ app: XCUIApplication, title: XCUIElement) {
        if title.exists {
            let back = title.buttons["BackButton"]
            if back.exists { back.tap() }
        }
        XCTAssertTrue(app.buttons["Active server, mbp"].waitForExistence(timeout: 10))
        attachScreenshotOnly(named: "visual refinement returned to mbp home")
    }

    private func waitForCondition(timeout: TimeInterval, _ predicate: () -> Bool) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if predicate() { return true }
            RunLoop.current.run(until: Date().addingTimeInterval(0.25))
        }
        return predicate()
    }

    private func waitForTranscriptContent(_ app: XCUIApplication, timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if app.scrollViews.firstMatch.exists { return true }
            if app.staticTexts["Couldn't Open Session"].exists { return false }
            RunLoop.current.run(until: Date().addingTimeInterval(0.5))
        }
        return app.scrollViews.firstMatch.exists
    }

    private func addKeptAttachment(_ text: String) {
        let attachment = XCTAttachment(string: text)
        attachment.lifetime = .keepAlways
        add(attachment)
    }

    private func attachScreenshotOnly(named name: String) {
        let screen = XCTAttachment(screenshot: XCUIScreen.main.screenshot())
        screen.name = name
        screen.lifetime = .keepAlways
        add(screen)
    }

    private func attachCurrentScreen(_ app: XCUIApplication, named name: String) {
        let tree = XCTAttachment(string: app.debugDescription)
        tree.name = "\(name) accessibility tree"
        tree.lifetime = .keepAlways
        add(tree)

        let screen = XCTAttachment(screenshot: XCUIScreen.main.screenshot())
        screen.name = name
        screen.lifetime = .keepAlways
        add(screen)
    }
}
