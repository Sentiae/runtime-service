#!/usr/bin/env python3
"""
End-to-end test: Start a Firecracker VM, connect via vsock, execute Python code.

Wire format: 4-byte big-endian length prefix + protobuf bytes
vsock handshake: send "CONNECT 52\n", expect "OK <port>\n"
"""

import json
import os
import socket
import struct
import subprocess
import sys
import tempfile
import time
import uuid

# ============================================================================
# Manual protobuf message construction (no compiled stubs needed)
# ============================================================================

def encode_varint(value):
    """Encode an integer as a protobuf varint."""
    result = b""
    while value > 0x7F:
        result += bytes([(value & 0x7F) | 0x80])
        value >>= 7
    result += bytes([value & 0x7F])
    return result

def encode_length_delimited(field_number, data):
    """Encode a length-delimited field (string, bytes, embedded message)."""
    tag = (field_number << 3) | 2
    return encode_varint(tag) + encode_varint(len(data)) + data

def encode_varint_field(field_number, value):
    """Encode a varint field (int32, int64, bool)."""
    tag = (field_number << 3) | 0
    return encode_varint(tag) + encode_varint(value)

def build_execute_task(task_id, language, code, timeout_secs=30):
    """Build an ExecuteTask protobuf message."""
    msg = b""
    msg += encode_length_delimited(1, task_id.encode("utf-8"))   # task_id
    msg += encode_length_delimited(2, language.encode("utf-8"))  # language
    msg += encode_length_delimited(3, code.encode("utf-8"))      # code
    # stdin = field 4, skip
    msg += encode_varint_field(5, timeout_secs)                  # timeout_secs
    return msg

def build_host_message_execute(task_id, language, code, timeout_secs=30):
    """Build a HostMessage with execute_task oneof."""
    execute_task = build_execute_task(task_id, language, code, timeout_secs)
    # execute_task is field 1 in HostMessage oneof
    return encode_length_delimited(1, execute_task)

def build_host_message_ping(timestamp):
    """Build a HostMessage with ping oneof."""
    ping = encode_varint_field(1, timestamp)  # Ping.timestamp = field 1
    return encode_length_delimited(4, ping)    # ping is field 4 in HostMessage

def build_host_message_shutdown():
    """Build a HostMessage with shutdown oneof."""
    # Shutdown is an empty message, field 3 in HostMessage
    return encode_length_delimited(3, b"")

# ============================================================================
# Protobuf decoding (minimal, field-level)
# ============================================================================

def decode_varint(data, pos):
    """Decode a varint from data at pos, return (value, new_pos)."""
    result = 0
    shift = 0
    while True:
        b = data[pos]
        result |= (b & 0x7F) << shift
        pos += 1
        if (b & 0x80) == 0:
            break
        shift += 7
    return result, pos

def decode_fields(data):
    """Decode protobuf binary into a list of (field_number, wire_type, value)."""
    fields = []
    pos = 0
    while pos < len(data):
        tag, pos = decode_varint(data, pos)
        field_number = tag >> 3
        wire_type = tag & 0x07
        if wire_type == 0:  # varint
            value, pos = decode_varint(data, pos)
            fields.append((field_number, wire_type, value))
        elif wire_type == 2:  # length-delimited
            length, pos = decode_varint(data, pos)
            value = data[pos:pos + length]
            pos += length
            fields.append((field_number, wire_type, value))
        elif wire_type == 1:  # 64-bit
            value = data[pos:pos + 8]
            pos += 8
            fields.append((field_number, wire_type, value))
        elif wire_type == 5:  # 32-bit
            value = data[pos:pos + 4]
            pos += 4
            fields.append((field_number, wire_type, value))
        else:
            raise ValueError(f"Unknown wire type {wire_type} at pos {pos}")
    return fields

def decode_guest_message(data):
    """
    Decode a GuestMessage. Returns (type_name, parsed_dict).
    GuestMessage oneof:
      1 = Ready, 2 = TaskOutput, 3 = TaskComplete, 4 = Metrics, 5 = Pong
    """
    fields = decode_fields(data)
    for field_number, wire_type, value in fields:
        if field_number == 1:  # Ready
            return "Ready", decode_ready(value)
        elif field_number == 2:  # TaskOutput
            return "TaskOutput", decode_task_output(value)
        elif field_number == 3:  # TaskComplete
            return "TaskComplete", decode_task_complete(value)
        elif field_number == 4:  # Metrics
            return "Metrics", {"raw": value.hex()}
        elif field_number == 5:  # Pong
            return "Pong", decode_pong(value)
    return "Unknown", {}

def decode_ready(data):
    fields = decode_fields(data)
    result = {"agent_version": "", "hostname": "", "supported_languages": []}
    for fn, wt, val in fields:
        if fn == 1:
            result["agent_version"] = val.decode("utf-8")
        elif fn == 2:
            result["hostname"] = val.decode("utf-8")
        elif fn == 3:
            result["supported_languages"].append(val.decode("utf-8"))
    return result

def decode_task_output(data):
    fields = decode_fields(data)
    result = {"task_id": "", "stream": "STDOUT", "data": b""}
    for fn, wt, val in fields:
        if fn == 1:
            result["task_id"] = val.decode("utf-8")
        elif fn == 2:
            result["stream"] = "STDERR" if val == 1 else "STDOUT"
        elif fn == 3:
            result["data"] = val
    return result

def decode_task_complete(data):
    fields = decode_fields(data)
    result = {"task_id": "", "exit_code": 0, "error": "", "duration_ms": 0,
              "cpu_time_ms": 0, "memory_peak_kb": 0}
    for fn, wt, val in fields:
        if fn == 1:
            result["task_id"] = val.decode("utf-8")
        elif fn == 2:
            result["exit_code"] = val
        elif fn == 3:
            result["error"] = val.decode("utf-8")
        elif fn == 4:
            result["duration_ms"] = val
        elif fn == 5:
            result["cpu_time_ms"] = val
        elif fn == 6:
            result["memory_peak_kb"] = val
    return result

def decode_pong(data):
    fields = decode_fields(data)
    result = {"timestamp": 0, "status": ""}
    for fn, wt, val in fields:
        if fn == 1:
            result["timestamp"] = val
        elif fn == 2:
            result["status"] = val.decode("utf-8")
    return result

# ============================================================================
# Wire protocol helpers
# ============================================================================

class BufferedSocket:
    """Socket wrapper that supports prepending leftover bytes from handshake."""
    def __init__(self, sock, extra_bytes=b""):
        self.sock = sock
        self.buf = extra_bytes

    def settimeout(self, t):
        self.sock.settimeout(t)

    def sendall(self, data):
        self.sock.sendall(data)

    def recv_exact(self, n, timeout=15):
        self.sock.settimeout(timeout)
        while len(self.buf) < n:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise ConnectionError("Connection closed")
            self.buf += chunk
        result = self.buf[:n]
        self.buf = self.buf[n:]
        return result

    def close(self):
        self.sock.close()

def send_message(sock, protobuf_bytes):
    """Send a length-prefixed protobuf message."""
    header = struct.pack("!I", len(protobuf_bytes))
    sock.sendall(header + protobuf_bytes)

def recv_message(sock, timeout=15):
    """Receive a length-prefixed protobuf message. Returns raw bytes."""
    if isinstance(sock, BufferedSocket):
        header = sock.recv_exact(4, timeout)
        length = struct.unpack("!I", header)[0]
        if length > 10 * 1024 * 1024:
            raise ValueError(f"Message too large: {length} bytes")
        body = sock.recv_exact(length, timeout)
        return body

    sock.settimeout(timeout)
    # Read 4-byte length header
    header = b""
    while len(header) < 4:
        chunk = sock.recv(4 - len(header))
        if not chunk:
            raise ConnectionError("Connection closed while reading header")
        header += chunk

    length = struct.unpack("!I", header)[0]
    if length > 10 * 1024 * 1024:  # 10MB sanity check
        raise ValueError(f"Message too large: {length} bytes")

    # Read the body
    body = b""
    while len(body) < length:
        chunk = sock.recv(length - len(body))
        if not chunk:
            raise ConnectionError("Connection closed while reading body")
        body += chunk

    return body

# ============================================================================
# Firecracker VM management
# ============================================================================

class FirecrackerVM:
    def __init__(self, vm_id=None):
        self.vm_id = vm_id or str(uuid.uuid4())[:8]
        self.socket_path = f"/tmp/firecracker-{self.vm_id}.sock"
        self.vsock_path = f"/tmp/firecracker-{self.vm_id}.vsock"
        self.log_path = f"/tmp/firecracker-{self.vm_id}.log"
        self.process = None
        self.cid = 3  # Guest CID

    def start(self):
        """Start the Firecracker VMM process."""
        print(f"[*] Starting Firecracker VM (id={self.vm_id})...")

        # Clean up any stale socket files
        for path in [self.socket_path, self.vsock_path, f"{self.vsock_path}_52"]:
            if os.path.exists(path):
                os.unlink(path)

        # Start firecracker process
        self.process = subprocess.Popen(
            [
                "/usr/local/bin/firecracker",
                "--api-sock", self.socket_path,
            ],
            stdout=open(self.log_path, "w"),
            stderr=subprocess.STDOUT,
        )
        time.sleep(0.5)

        if self.process.poll() is not None:
            with open(self.log_path) as f:
                print(f"[!] Firecracker exited early: {f.read()}")
            raise RuntimeError("Firecracker process died")

        print(f"[*] Firecracker PID: {self.process.pid}")

        # Configure the VM via API
        self._api_put("/boot-source", {
            "kernel_image_path": "/var/lib/firecracker/kernels/vmlinux-5.10.bin",
            "boot_args": "console=ttyS0 reboot=k panic=1 pci=off"
        })

        # Create a writable overlay copy of the rootfs
        self.rootfs_overlay = f"/tmp/firecracker-{self.vm_id}-rootfs.ext4"
        subprocess.run(["cp", "/var/lib/firecracker/rootfs/python.ext4", self.rootfs_overlay], check=True)

        self._api_put("/drives/rootfs", {
            "drive_id": "rootfs",
            "path_on_host": self.rootfs_overlay,
            "is_root_device": True,
            "is_read_only": False
        })

        self._api_put("/machine-config", {
            "vcpu_count": 2,
            "mem_size_mib": 256
        })

        self._api_put("/vsock", {
            "guest_cid": self.cid,
            "uds_path": self.vsock_path
        })

        # Start the VM
        self._api_put("/actions", {"action_type": "InstanceStart"})
        print("[*] VM instance started")

    def _api_put(self, path, data):
        """Make a PUT request to the Firecracker API socket."""
        import http.client
        import json as json_mod

        # Use curl since http.client doesn't support Unix sockets easily
        body = json_mod.dumps(data)
        result = subprocess.run(
            ["curl", "--unix-socket", self.socket_path,
             "-s", "-X", "PUT",
             f"http://localhost{path}",
             "-H", "Content-Type: application/json",
             "-d", body],
            capture_output=True, text=True
        )
        if result.returncode != 0:
            raise RuntimeError(f"API call failed: {result.stderr}")
        if result.stdout and '"fault_message"' in result.stdout:
            raise RuntimeError(f"API error on {path}: {result.stdout}")
        print(f"    PUT {path}: OK")
        return result.stdout

    def connect_vsock(self, port=52, timeout=30):
        """Connect to the guest agent via vsock UDS with CONNECT handshake.

        Matches the Go implementation: connect to the vsock UDS path directly,
        send CONNECT <port>, retry until OK (guest agent needs time to boot
        and start listening).
        """
        vsock_uds = self.vsock_path
        print(f"[*] Connecting to vsock UDS at {vsock_uds} (port {port})...")

        deadline = time.time() + timeout
        sock = None
        response_str = ""

        while time.time() < deadline:
            # Ensure we have a connection
            if sock is None:
                try:
                    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                    sock.settimeout(2)
                    sock.connect(vsock_uds)
                except (socket.error, OSError) as e:
                    print(f"    connect attempt failed: {e}, retrying...")
                    sock = None
                    time.sleep(0.5)
                    continue

            # Send CONNECT handshake
            try:
                sock.sendall(f"CONNECT {port}\n".encode())
            except (socket.error, OSError):
                sock.close()
                sock = None
                time.sleep(0.5)
                continue

            # Read response line using buffered reader approach
            try:
                sock.settimeout(2)
                response = b""
                while b"\n" not in response:
                    chunk = sock.recv(256)
                    if not chunk:
                        raise ConnectionError("closed")
                    response += chunk
            except (socket.error, ConnectionError, OSError):
                sock.close()
                sock = None
                time.sleep(0.5)
                continue

            response_str = response.decode().strip()
            if response_str.startswith("OK"):
                print(f"[*] vsock handshake response: {response_str}")
                sock.settimeout(None)
                # Wrap with buffered socket, preserving any extra bytes
                # received after the OK line (Ready message may be buffered)
                newline_pos = response.index(b"\n") + 1
                extra = response[newline_pos:]
                if extra:
                    print(f"    ({len(extra)} extra bytes buffered after handshake)")
                return BufferedSocket(sock, extra)
            else:
                print(f"    handshake got: {response_str!r}, retrying...")
                sock.close()
                sock = None
                time.sleep(0.5)

        raise TimeoutError(f"vsock handshake timed out after {timeout}s")

    def stop(self):
        """Stop the VM and clean up."""
        print("[*] Stopping VM...")
        if self.process:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
        # Cleanup
        for path in [self.socket_path, self.vsock_path,
                      f"{self.vsock_path}_52", self.rootfs_overlay]:
            if path and os.path.exists(path):
                try:
                    os.unlink(path)
                except OSError:
                    pass
        print("[*] VM stopped and cleaned up")


# ============================================================================
# Test execution
# ============================================================================

def run_task(sock, task_id, code, language="python"):
    """Send a task and collect all responses until TaskComplete."""
    print(f"\n[>] Sending task '{task_id}': {code[:60]}...")
    msg = build_host_message_execute(task_id, language, code, timeout_secs=30)
    send_message(sock, msg)

    outputs = []
    complete = None

    while True:
        raw = recv_message(sock, timeout=15)
        msg_type, parsed = decode_guest_message(raw)
        print(f"    [{msg_type}] {parsed}")

        if msg_type == "TaskOutput":
            outputs.append(parsed)
        elif msg_type == "TaskComplete":
            complete = parsed
            break
        elif msg_type == "Metrics":
            continue  # Skip periodic metrics
        else:
            print(f"    (unexpected message type: {msg_type})")

    return outputs, complete


def main():
    print("=" * 70)
    print("Firecracker E2E Test")
    print("=" * 70)

    vm = FirecrackerVM()
    try:
        # Step 1: Start the VM
        vm.start()

        # Step 2: Connect via vsock
        sock = vm.connect_vsock(port=52, timeout=30)

        # Step 3: Read Ready message
        print("\n[*] Waiting for Ready message...")
        raw = recv_message(sock, timeout=30)
        msg_type, ready_info = decode_guest_message(raw)
        print(f"[*] Got: {msg_type} -> {ready_info}")
        assert msg_type == "Ready", f"Expected Ready, got {msg_type}"
        print("[OK] Agent is ready!")

        # Step 4: Execute first task
        outputs1, complete1 = run_task(
            sock,
            task_id="test-001",
            code='print("hello from firecracker")'
        )

        stdout1 = b"".join(o["data"] for o in outputs1 if o["stream"] == "STDOUT")
        print(f"\n[RESULT] Task 1 stdout: {stdout1.decode().strip()!r}")
        print(f"[RESULT] Task 1 exit code: {complete1['exit_code']}")
        print(f"[RESULT] Task 1 duration: {complete1['duration_ms']}ms")

        assert complete1["exit_code"] == 0, f"Task 1 failed with exit code {complete1['exit_code']}"
        assert "hello from firecracker" in stdout1.decode(), "Expected output not found"
        print("[OK] Task 1 passed!")

        # Step 5: Execute a second task on the same connection (warm reuse)
        outputs2, complete2 = run_task(
            sock,
            task_id="test-002",
            code='import sys; print(f"Python {sys.version}"); print(f"2+2={2+2}")'
        )

        stdout2 = b"".join(o["data"] for o in outputs2 if o["stream"] == "STDOUT")
        print(f"\n[RESULT] Task 2 stdout: {stdout2.decode().strip()!r}")
        print(f"[RESULT] Task 2 exit code: {complete2['exit_code']}")
        print("[OK] Task 2 passed (same connection)!")

        # Step 6: Disconnect and reconnect (test warm pool reuse)
        print("\n[*] Disconnecting...")
        sock.close()
        time.sleep(1)

        print("[*] Reconnecting for warm reuse test...")
        sock2 = vm.connect_vsock(port=52, timeout=10)

        # On reconnect we should get a Ready message again
        print("[*] Waiting for Ready on reconnect...")
        raw = recv_message(sock2, timeout=15)
        msg_type, ready_info2 = decode_guest_message(raw)
        print(f"[*] Reconnect got: {msg_type} -> {ready_info2}")

        # Execute task on new connection
        outputs3, complete3 = run_task(
            sock2,
            task_id="test-003",
            code='import os; print(f"PID={os.getpid()}, reuse works!")'
        )

        stdout3 = b"".join(o["data"] for o in outputs3 if o["stream"] == "STDOUT")
        print(f"\n[RESULT] Task 3 stdout: {stdout3.decode().strip()!r}")
        print(f"[RESULT] Task 3 exit code: {complete3['exit_code']}")
        print("[OK] Task 3 passed (reconnect)!")

        # Step 7: Test error handling - code that raises an exception
        outputs4, complete4 = run_task(
            sock2,
            task_id="test-004",
            code='raise ValueError("intentional error")'
        )

        stderr4 = b"".join(o["data"] for o in outputs4 if o["stream"] == "STDERR")
        print(f"\n[RESULT] Task 4 stderr: {stderr4.decode().strip()!r}")
        print(f"[RESULT] Task 4 exit code: {complete4['exit_code']}")
        assert complete4["exit_code"] != 0, "Expected non-zero exit code for error task"
        print("[OK] Task 4 passed (error handling)!")

        sock2.close()

        print("\n" + "=" * 70)
        print("ALL TESTS PASSED")
        print("=" * 70)

    except Exception as e:
        print(f"\n[FAIL] {type(e).__name__}: {e}")
        import traceback
        traceback.print_exc()

        # Dump VM log for debugging
        if os.path.exists(vm.log_path):
            print(f"\n--- Firecracker log ({vm.log_path}) ---")
            with open(vm.log_path) as f:
                print(f.read()[-2000:])  # Last 2000 chars
        sys.exit(1)
    finally:
        vm.stop()


if __name__ == "__main__":
    main()
