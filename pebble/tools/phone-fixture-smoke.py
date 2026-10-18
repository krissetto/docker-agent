#!/usr/bin/env python3
"""Run synthetic, credential-free AppMessage frames against the native Emery app."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import sys
import tempfile
import time
import uuid
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TOOLS = ROOT / ".tools"
SDK_HOME = TOOLS / "pebble-sdk"
SDK_VERSION = "4.33.1"
PBW_UUID = uuid.UUID("27cd8c09-4eb0-4673-8ac7-a4639cc92c38")
FIXTURES = Path(__file__).with_name("phone-fixtures.json")

os.environ.update({
    "HOME": str(TOOLS / "home"),
    "PEBBLE_SDK_HOME": str(SDK_HOME),
    "PEBBLE_SDK_LINK": str(TOOLS / "sdk-runtime-link"),
    "PEBBLE_EXTRA_PATH": str(SDK_HOME / "SDKs" / SDK_VERSION / "toolchain" / "bin"),
    "PEBBLE_QEMU_PATH": str(SDK_HOME / "SDKs" / SDK_VERSION / "toolchain" / "bin" / "qemu-pebble"),
})
LOCAL_PYTHON = TOOLS / "tool-dir" / "pebble-tool" / "bin" / "python"
if LOCAL_PYTHON.exists() and sys.version_info[:2] != (3, 13) and os.environ.get("PEBBLE_FIXTURE_REEXEC") != "1":
    os.environ["PEBBLE_FIXTURE_REEXEC"] = "1"
    os.execv(str(LOCAL_PYTHON), [str(LOCAL_PYTHON), str(Path(__file__).resolve()), *sys.argv[1:]])


def setup_api():
    if not LOCAL_PYTHON.exists():
        raise RuntimeError("Run pebble/tools/setup.sh first")
    sys.path.insert(0, str(TOOLS / "tool-dir" / "pebble-tool" / "lib" / "python3.13" / "site-packages"))
    from PIL import Image
    from libpebble2.communication import PebbleConnection
    from libpebble2.communication.transports.qemu import MessageTargetQemu, QemuTransport
    from libpebble2.communication.transports.qemu.protocol import QemuBluetoothConnection, QemuButton
    from libpebble2.protocol.apps import AppRunState, AppRunStateRequest
    from libpebble2.services.appmessage import AppMessageService, CString, Uint32
    from libpebble2.services.screenshot import Screenshot
    return locals()


def load_bundle():
    path = ROOT / "watch/app/build/app.pbw"
    if not path.is_file():
        raise RuntimeError("Build the watchapp first with ./pebble/tools/build.sh")
    sources = ("watch/app/src/c/app.c", "watch/app/src/c/protocol.h", "watch/app/protocol.json",
               "watch/app/package.json", "watch/app/src/pkjs/index.js", "watch/app/config.html")
    newer = [str((ROOT / rel).relative_to(ROOT)) for rel in sources if (ROOT / rel).exists() and (ROOT / rel).stat().st_mtime > path.stat().st_mtime]
    if newer:
        raise RuntimeError("PBW predates current source: " + ", ".join(newer))
    with zipfile.ZipFile(path) as archive:
        info = json.loads(archive.read("appinfo.json"))
        protocol = json.loads((ROOT / "watch/app/protocol.json").read_text())
        keys = info.get("messageKeys") or info.get("appKeys")
        missing = set(protocol["keys"]) - set(keys)
        if missing:
            raise RuntimeError("Generated PBW key map misses " + ", ".join(sorted(missing)))
        if info.get("uuid") != str(PBW_UUID):
            raise RuntimeError("Built PBW UUID does not match fixture target")
    return path, {name: int(key) for name, key in keys.items()}


def dictionary_size(frame, request_id):
    payload = {"type": 2, "request_id": request_id, **frame}
    # AppMessage dictionary is one count byte plus 7-byte tuple headers and data.
    size = 1
    for value in payload.values():
        if isinstance(value, str):
            data_bytes = len(value.encode("utf-8")) + 1
        elif isinstance(value, (int, bool)):
            data_bytes = 4  # CLI-compatible uint32 fields, including C bool decoder.
        else:
            raise TypeError(f"unexpected frame value {value!r}")
        if data_bytes > 255:
            raise ValueError("single AppMessage value exceeds 255 bytes")
        size += 7 + data_bytes
    if size > 1024:
        raise ValueError(f"dictionary {size} bytes exceeds C target min/negotiated inbox 1024")
    return size


def pid_running(pid):
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def running_emulator():
    state_path = Path(tempfile.gettempdir()) / "pb-emulator.json"
    if not state_path.exists():
        raise RuntimeError(f"No pebble-tool emulator state file {state_path}")
    state = json.loads(state_path.read_text())
    info = state.get("emery", {}).get(SDK_VERSION)
    if not info or not pid_running(info["qemu"]["pid"]) or not pid_running(info["pypkjs"]["pid"]):
        raise RuntimeError("No live Emery 4.33.1 emulator; run the standard pebble install --emulator emery command")
    return state_path, info


def stop_phone_simulator(pid):
    command = subprocess.check_output(["ps", "-p", str(pid), "-o", "command="], text=True).strip()
    if "-m pypkjs" not in command:
        raise RuntimeError("Refusing to signal process that is not the SDK pypkjs simulator")
    os.kill(pid, signal.SIGTERM)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline and pid_running(pid):
        time.sleep(.05)
    if pid_running(pid):
        raise RuntimeError("SDK pypkjs simulator did not stop cleanly")


def take_capture(connection, api, path):
    rows = api["Screenshot"](connection).grab_image()
    width, height = len(rows[0]) // 3, len(rows)
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    api["Image"].frombytes("RGB", (width, height), b"".join(bytes(row) for row in rows)).save(target)
    print(f"Screenshot {target} {width}x{height}")


def click(connection, api, button, duration=.12):
    target = api["MessageTargetQemu"]()
    connection.transport.send_packet(api["QemuButton"](state=button), target=target)
    time.sleep(duration)
    connection.transport.send_packet(api["QemuButton"](state=0), target=target)
    time.sleep(.25)


def ack_matches(queue_, request_id, frame, index, timeout=4):
    expected = {"type": 9, "request_id": request_id, "frame": frame, "index": index}
    try:
        actual = queue_.get(timeout=timeout)
    except queue.Empty as exc:
        raise TimeoutError(f"No native C ack for {expected}") from exc
    if actual != expected:
        raise RuntimeError(f"Native C ack mismatch: expected {expected}, got {actual}")
    return actual


def wait_for_command(command_queue, wanted_type, timeout=7):
    deadline = time.monotonic() + timeout
    deferred = []
    try:
        while time.monotonic() < deadline:
            try:
                packet = command_queue.get(timeout=max(.1, deadline - time.monotonic()))
            except queue.Empty:
                break
            if packet.get("type") == wanted_type:
                return packet
            deferred.append(packet)
        raise TimeoutError(f"Watch did not send expected command type={wanted_type}; skipped={deferred}")
    finally:
        for packet in deferred:
            command_queue.put(packet)


def transmit_scenario(connection, service, scenario, request_id, key_ids, api, reverse_keys, log):
    for frame in scenario["frames"]:
        payload = {"type": 2, "request_id": request_id, **frame}
        values = {key_ids[name]: (api["Uint32"](int(value)) if isinstance(value, (bool, int)) else api["CString"](value))
                  for name, value in payload.items()}
        size = dictionary_size(frame, request_id)
        transaction = service.send_message(PBW_UUID, values)
        # There can be native outbox acks interleaved with phone->watch transport acks;
        # ignore unrelated transaction IDs but require this exact tuple's AppMessage ack.
        deadline = time.monotonic() + 4
        while time.monotonic() < deadline:
            try:
                txid, target_uuid = api["transport_ack"].get(timeout=max(.05, deadline - time.monotonic()))
            except queue.Empty:
                break
            if txid == transaction and target_uuid == PBW_UUID:
                break
        else:
            raise TimeoutError(f"No transport ack for inbound tx={transaction}")
        ack = ack_matches(api["native_ack"], request_id, frame["frame"], frame["index"])
        log.append({"request_id":request_id,"frame":frame["frame"],"index":frame["index"],"dict_bytes":size,"native_ack":ack})
        if scenario["name"] == "chat" and frame["index"] == 0:
            duplicate_tx = service.send_message(PBW_UUID, values)
            deadline = time.monotonic() + 4
            while time.monotonic() < deadline:
                try:
                    txid, target_uuid = api["transport_ack"].get(timeout=max(.05, deadline - time.monotonic()))
                except queue.Empty:
                    break
                if txid == duplicate_tx and target_uuid == PBW_UUID:
                    break
            else:
                raise TimeoutError("Duplicate chat begin had no inbound transport ack")
            duplicate = ack_matches(api["native_ack"], request_id, frame["frame"], frame["index"])
            log.append({"request_id":request_id,"frame":frame["frame"],"index":frame["index"],"duplicate_ack":duplicate})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--list", action="store_true")
    parser.add_argument("--capture-dir", default="/tmp/pebble-native-smoke")
    parser.add_argument("--no-install", action="store_true")
    parser.add_argument("--leave-running", action="store_true")
    args = parser.parse_args()
    api = setup_api()
    fixture_document = json.loads(FIXTURES.read_text())
    fixtures = {scenario["name"]: scenario for scenario in fixture_document["scenarios"]}
    required = {"runtimes", "agents", "sessions", "chat", "invalid-order"}
    missing = required.difference(fixtures)
    if missing:
        raise RuntimeError("Missing phone fixture scenarios: " + ", ".join(sorted(missing)))
    if args.list:
        for scenario in fixture_document["scenarios"]:
            if scenario["name"] == "invalid-order":
                print("invalid-order: skip one physical frame index; no native ACK expected")
            else:
                sizes = [dictionary_size(frame, scenario["request_id"]) for frame in scenario["frames"]]
                print(f"{scenario['name']}: {len(sizes)} frames, largest {max(sizes)} byte dictionary")
        return

    pbw, key_ids = load_bundle()
    if not args.no_install:
        with tempfile.TemporaryDirectory(prefix="pebble-native-only-") as directory:
            native_pbw = Path(directory) / "native-only.pbw"
            with zipfile.ZipFile(pbw) as src, zipfile.ZipFile(native_pbw, "w") as dst:
                for item in src.infolist():
                    if item.filename not in {"pebble-js-app.js", "pebble-js-app.js.map"}:
                        dst.writestr(item, src.read(item.filename))
            subprocess.run([str(ROOT / "tools/pebble.sh"), "install", "--emulator", "emery", str(native_pbw)], check=True)

    _, emulator = running_emulator()
    stop_phone_simulator(int(emulator["pypkjs"]["pid"]))
    connection = None
    service = None
    api["commands"] = queue.Queue()
    api["native_ack"] = queue.Queue()
    api["transport_ack"] = queue.Queue()
    logs = []
    capture = Path(args.capture_dir)
    try:
        connection = api["PebbleConnection"](api["QemuTransport"]("127.0.0.1", int(emulator["qemu"]["port"])))
        connection.connect()
        service = api["AppMessageService"](connection)

        def received(txid, app_uuid, values):
            if app_uuid != PBW_UUID:
                return
            names = {key_ids_inv[key]: val for key,val in values.items()} if False else values
            values = {name: values.get(key_id) for name,key_id in key_ids.items() if key_id in values}
            if values.get("type") == 9:
                api["native_ack"].put(values)
            else:
                api["commands"].put(values)

        def inbound_ack(txid, app_uuid):
            api["transport_ack"].put((txid, app_uuid))

        service.register_handler("appmessage", received)
        service.register_handler("ack", inbound_ack)
        connection.run_async()
        if connection.watch_platform != "emery":
            raise RuntimeError(f"Unexpected platform {connection.watch_platform}")
        connection.transport.send_packet(api["QemuBluetoothConnection"](connected=True), target=api["MessageTargetQemu"]())
        time.sleep(.5)
        connection.transport.send_packet(api["QemuBluetoothConnection"](connected=False), target=api["MessageTargetQemu"]())
        time.sleep(.2)
        connection.transport.send_packet(api["QemuBluetoothConnection"](connected=True), target=api["MessageTargetQemu"]())

        request = wait_for_command(api["commands"], 1)
        sequence=["runtimes"]
        transmit_scenario(connection,service,fixtures["runtimes"],request["request_id"],key_ids,api,None,logs)
        take_capture(connection,api,capture/"runtimes.png")
        click(connection,api,api["QemuButton"].Button.Select)
        request=wait_for_command(api["commands"],3)
        transmit_scenario(connection,service,fixtures["agents"],request["request_id"],key_ids,api,None,logs)
        take_capture(connection,api,capture/"agents.png")
        click(connection,api,api["QemuButton"].Button.Select)
        request=wait_for_command(api["commands"],3)
        if request.get("source") != "workspace-source.yaml" or request.get("agent_name") != "orchestrator":
            raise RuntimeError(f"Native source/root selection fixture differed: {request}")
        transmit_scenario(connection,service,fixtures["sessions"],request["request_id"],key_ids,api,None,logs)
        take_capture(connection,api,capture/"sessions.png")
        click(connection,api,api["QemuButton"].Button.Down)
        click(connection,api,api["QemuButton"].Button.Select)
        request=wait_for_command(api["commands"],4)
        if request.get("session_id") != "session-7a": raise RuntimeError(f"Wrong native session selection: {request}")
        transmit_scenario(connection,service,fixtures["chat"],request["request_id"],key_ids,api,None,logs)
        take_capture(connection,api,capture/"chat.png")
        for button in (api["QemuButton"].Button.Select,api["QemuButton"].Button.Down,api["QemuButton"].Button.Select,api["QemuButton"].Button.Select): click(connection,api,button)
        take_capture(connection,api,capture/"review.png")
        click(connection,api,api["QemuButton"].Button.Back)
        click(connection,api,api["QemuButton"].Button.Down,duration=.7)
        take_capture(connection,api,capture/"status-heap.png")
        click(connection,api,api["QemuButton"].Button.Back)

        click(connection,api,api["QemuButton"].Button.Back)
        click(connection,api,api["QemuButton"].Button.Back)
        request=wait_for_command(api["commands"],3)
        if request.get("source") != "workspace-source.yaml": raise RuntimeError(f"Native refresh did not preserve source: {request}")
        transmit_scenario(connection,service,fixtures["agents"],request["request_id"],key_ids,api,None,logs)
        click(connection,api,api["QemuButton"].Button.Select)
        request=wait_for_command(api["commands"],3)
        malformed=fixtures["invalid-order"]
        start=malformed["frames"][0]; end=malformed["frames"][1]
        transmit_scenario(connection,service,{"name":"invalid-start","frames":[start]},request["request_id"],key_ids,api,None,logs)
        size=dictionary_size(end,request["request_id"])
        values={key_ids[k]: api["Uint32"](int(v)) if isinstance(v,(bool,int)) else api["CString"](v) for k,v in {"type":2,"request_id":request["request_id"],**end}.items()}
        txid=service.send_message(PBW_UUID,values)
        deadline=time.monotonic()+4
        while time.monotonic()<deadline:
            try: found=api["transport_ack"].get(timeout=max(.05,deadline-time.monotonic()))
            except queue.Empty: break
            if found==(txid,PBW_UUID): break
        else: raise TimeoutError("Malformed frame did not receive transport-level AppMessage ack")
        try: unexpected=api["native_ack"].get(timeout=.8)
        except queue.Empty: unexpected=None
        if unexpected is not None: raise RuntimeError(f"Out-of-order frame was unexpectedly protocol-ACKed: {unexpected}")
        logs.append({"scenario":"invalid-order","index":end["index"],"native_ack":"correctly ignored","dict_bytes":size})
        take_capture(connection,api,capture/"invalid-order.png")
        print(json.dumps({"platform":"emery","app_uuid":str(PBW_UUID),"results":logs,"screenshots":str(capture)},indent=2))
    finally:
        if service: service.shutdown()
        if not args.leave_running: subprocess.run([str(ROOT/"tools/pebble.sh"),"kill"],check=False)


if __name__ == "__main__":
    main()
