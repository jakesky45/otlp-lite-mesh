# Specification: OTLP-Lite-Mesh Protocol
**Version:** 1.0.0-rc1
**Status:** Release Candidate
**Domain:** Tactical Systems / Infrastructure-Free Observability

> Sections 1–6 are the spec as received. Section 7 ("Errata and implementation decisions") is ours:
> every place the RC is ambiguous or wrong, what we decided, and why. It grows as the stages land.

---

## 1. Executive Summary & Intent

The **OpenTelemetry Protocol over Mesh (OTLP-Lite-Mesh)** provides a highly deterministic, low-overhead binary transport wrapper designed to route standardized telemetry metrics, logs, and distributed traces across infrastructure-free, peer-to-peer radio meshes (e.g., Wi-Fi Aware, Wi-Fi Direct, Bluetooth LE, and LoRa networks).

Standard cloud-native OTLP relies on robust application layers like HTTP/JSON or gRPC over persistent TCP connections. On ad-hoc mesh networks, the packet transmission overhead, connection handshakes, and chatty protocol negotiations quickly break the physical limitations of the airwaves. This specification replaces transport-layer negotiation with a zero-handshake, stateless binary framing layout that maximizes payload density while preserving OpenTelemetry backend compatibility.

---

## 2. System Architecture

An OTLP-Lite-Mesh deployment consists of three distinct topology roles:

```
┌────────────────────────┐
│  Edge Node (Mobile)    │
│  [ Instrumented App ]  │
│           │            │
│  (Custom Mesh Exporter)│
└───────────┬────────────┘
            │
            ▼ (Raw OTLP-Lite Packets / UDP / Radio Broadcast)
(((( ░░░░░  INFRASTRUCTURE-FREE WIRELESS MESH  ░░░░░ ))))
            │
            ├─────────────────────────────────┐
            ▼                                 ▼
┌────────────────────────┐       ┌────────────────────────┐
│   Relay Node (Peer)    │       │ Gateway Node (Leader)  │
│  [ Stores & Forwards ] │       │ [ Cellular/Satellite ] │
└───────────┬────────────┘       └────────────┬───────────┘
            │                                 │
            └───────(Mesh Routing Hop)────────┘
                                              │
                                              ▼ (Standard OTLP/gRPC)
                                 ┌────────────────────────┐
                                 │ Cloud Backend Engine   │
                                 │ (Jaeger/Grafana/Mimir) │
                                 └────────────────────────┘
```

### 2.1 Role Definitions
*   **Edge Node:** Resource-constrained devices running business logic. They capture local tracing/metrics data, aggressively strip non-essential baggage, encode it into the `OTLP-Lite-Mesh` wire format, and broadcast it.
*   **Relay Node:** Mesh participants that catch P2P frames and forward them down the line based on mesh-routing rules. They do not unpack or decode the inner payload.
*   **Gateway Node:** Nodes possessing both a mesh radio interface and an active internet backhaul uplink. They extract the payload, hydrate the frames back into standard enterprise OTLP schemas, and pass them upstream.

---

## 3. Wire Format Specification

Every network frame transmitted over the mesh interface must strictly implement the following fixed-width binary layout prior to appending the payload. All multi-byte integers are encoded using **Network Byte Order (Big Endian)**.

### 3.1 Binary Frame Layout

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|   MagicByte   |    Version    |   DataType    |     Flags     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                                                               |
+                            NodeID                             +
|                          (16 Bytes)                           |
+                                                               +
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                           PayloadLen                          |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                                                               |
+                        Payload Bytes...                       +
|                          (Variable)                           |
```

### 3.2 Field Data Dictionary

| Byte Offset | Field Identifier | Type | Bitmask / Enumeration Mapping |
| :--- | :--- | :--- | :--- |
| `0x00` | `MagicByte` | `uint8` | Constant signature identifier: `0x4F` (ASCII character 'O') |
| `0x01` | `Version` | `uint8` | Reflects protocol layout evolution. Current version: `0x01` |
| `0x02` | `DataType` | `uint8` | `0x01` = OTLP Log Record<br>`0x02` = OTLP Metric Data<br>`0x03` = OTLP Span (Trace) |
| `0x03` | `Flags` | `uint8` | **Bit 0 (LSB):** Compression indicator (`0` = Uncompressed, `1` = Compressed)<br>**Bit 1:** Compression Algorithm (`0` = Gzip, `1` = Zstd)<br>**Bits 2–7:** Reserved for future network routing extensions. |
| `0x04 - 0x13`| `NodeID` | `16-bytes`| Unique cryptographic or hardware-backed UUID of the originating device. |
| `0x14 - 0x17`| `PayloadLen`| `uint32` | Absolute byte size (N) of the following serialized payload array. |
| `0x18 - ...` | `Payload` | `bytes` | Raw serialized Protocol Buffer bytes representing the telemetry object. |

---

## 4. Payload Constraints & Payload Pruning Rules

To protect packets from splitting across radio MTU boundaries (e.g., keeping payloads under 1280 bytes for IPv6 ad-hoc networks or 256 bytes for LoRa frames), Edge Nodes must parse out and drop extraneous metadata prior to serialization.

### 4.1 Distributed Tracing Spans (`DataType = 0x03`)
*   **TraceID & SpanID Preservation:** The 16-byte `TraceID` and 8-byte `SpanID` *must* remain entirely untouched to prevent graph breaks in parent-child tracking layouts.
*   **Baggage / TraceState Drop:** The `TraceState` and all associated text-heavy context objects are completely dropped at the exporter layer.
*   **Attribute Capping:** Spans are restricted to a maximum of 3 highly compressed attribute fields. Recommended structural mappings include:
    *   `error.message` (Truncated to maximum 64 characters)
    *   `sys.battery` (Integer values)
    *   `sys.latency` (Millisecond durations)

### 4.2 Metrics Payload (`DataType = 0x02`)
*   **Allowed Data Types:** Only `Gauge` and `Sum` types are supported over the air.
*   **Prohibited Data Types:** Structural `Histogram` and `ExponentialHistogram` objects are strictly forbidden due to their multi-bucket arrays heavily exhausting airtime allocations.

---

## 5. Transport Layer Profiles

Depending on the underlying radio hardware interface, the protocol switches between two specific network delivery models:

### 5.1 Profile Alpha: IP-Based Ad-hoc Links (Wi-Fi Direct / Wi-Fi Aware)
*   **Transport Protocol:** Stateless User Datagram Protocol (UDP).
*   **Target Port Allocation:** Port `4319` (Registered protocol default).
*   **Transmission Type:** Unacknowledged Multicast / Subnet Directed Broadcast.

### 5.2 Profile Beta: Non-IP Personal Area Meshes (Bluetooth LE / Custom Radios)
*   **Transport Layer:** Native Bluetooth Attribute Protocol (ATT) or L2CAP Channels.
*   **MTU Adjustments:** Payloads exceeding the hardware link's MTU limitations must be split into sequential packets using a fragmentation index appended directly into the `Flags` bitmask field.

---

## 6. Gateway Deserialization & Cloud Ingestion Rules

When a Gateway Node captures an `OTLP-Lite-Mesh` frame, it must run the following unpacking pipeline:

1.  **Validation:** Verify that `MagicByte == 0x4F`. If false, drop the packet instantly to prevent buffer poisoning.
2.  **Safety Buffer Allocation:** Validate that the network packet's total byte length matches `24 + PayloadLen`.
3.  **Decompression:** Check the `Flags` byte. If compression bits are flagged, stream the payload segment through an in-memory decompression ring buffer.
4.  **Enrichment:** Instantiate a standard OpenTelemetry `ExportTraceServiceRequest` or `ExportMetricsServiceRequest` structure. Inject structural infrastructure tags highlighting how the data made it to the cloud:
    *   `mesh.gateway.id` = Local Gateway Identifier
    *   `mesh.node.id` = Retrieved `NodeID` mapped to string representation.
5.  **Ingestion:** Establish a standard TLS-encrypted upstream gRPC link and push the unified telemetry down to the production cloud infrastructure cluster.

---

## 7. Errata and implementation decisions (ours)

The RC leaves several things unsaid. These are the choices this implementation makes. Each is
referenced from the code and the blog posts by its number (E1, E2, ...).

| # | Spec gap | Decision | Why |
| :-- | :-- | :-- | :-- |
| **E1** | §3.2 says the payload is "raw serialized Protocol Buffer bytes representing the telemetry object" but never names the message type. | Exactly one message per frame: `DataType 0x01` → `opentelemetry.proto.logs.v1.LogRecord`, `0x02` → `metrics.v1.Metric`, `0x03` → `trace.v1.Span`. `Resource` and `InstrumentationScope` are **not** transmitted; the gateway synthesises them (`mesh.node.id`, `mesh.gateway.id`, `service.name`). | A `ResourceSpans` wrapper adds ~100+ bytes of repeated metadata per frame. The NodeID already identifies the source, so the gateway can rebuild the resource. |
| **E2** | §6.2 checks `len == 24 + PayloadLen` but sets no upper bound. | Receivers enforce `PayloadLen <= MaxPayload` (default 1256 so the whole frame fits in a 1280-byte IPv6 minimum MTU). Configurable per transport profile. | Without a bound, an attacker-controlled `PayloadLen` is exactly the "buffer poisoning" §6.1 worries about. |
| **E3** | §3.2 Flags: bit 1 is meaningless when bit 0 is clear; bits 2–7 "reserved" but no behaviour defined. | Bit 1 must be `0` when bit 0 is `0`. Bits 2–7 must be `0` in version 1; frames with any set are rejected. | Rejecting non-zero reserved bits is what lets a later version use them (e.g. for §5.2's fragmentation index) without old receivers mis-parsing. |
| **E4** | §5.1 calls port 4319 a "registered protocol default". | Use 4319 as the default but document that it is **not** IANA-registered. 4317 (OTLP/gRPC) and 4318 (OTLP/HTTP) are OTLP's conventional ports; 4319 just continues the sequence. | Honesty. Being adjacent to the OTLP ports is still a reasonable convention. |
| **E5** | §3.2 NodeID: "cryptographic or hardware-backed UUID" with no format. | 16 raw bytes, generated once per edge process as a UUIDv4 for the demo. The gateway renders it as the canonical 8-4-4-4-12 string in `mesh.node.id`. | Hardware-backed identity is out of scope for v0.1; UUIDv4 keeps the 16-byte width and string mapping the spec implies. |
| **E6** | §5.2 wants a fragmentation index "appended directly into the Flags bitmask" (6 spare bits = max 64 fragments) with no reassembly rules. | Deferred. Profile Beta is not implemented in v0.1. | Needs real BLE hardware to test honestly. Discussed in the wrap-up post. |
| **E8** | §6.3 decompresses flagged payloads with no bound on the inflated size. | Receivers decompress with a size limit (default `DefaultMaxPayload`, 1256 bytes). gzip stops reading one byte past the limit; zstd reads the declared content size from the frame header and refuses before allocating. Measured: 64 KB of zeros is 15 bytes of zstd on the wire. | A 15-byte frame that inflates 4000× is a decompression bomb; without a cap a handful of hostile packets exhaust a gateway's memory. Same class of problem as E2, one layer up. |
| **E7** | No confidentiality, integrity or authentication anywhere: any radio in range can read every frame or inject frames with any NodeID. | **Mandatory sealing extension.** Flags bit 2 = sealed. The payload (after compression) is encrypted with ChaCha20-Poly1305; the 24-byte header is authenticated as associated data so relays can still route on it. Nonce = NodeID[0:8] ‖ 4-byte per-node counter (counter sent in clear, first 4 bytes of the sealed payload) giving replay protection. Per-node keys derived from a mesh master key with HKDF-SHA256. Receivers reject unsealed frames by default. Overhead 20 bytes. | An unsecured telemetry protocol is not deployable, regardless of the airtime saved. This uses one of the "reserved for routing" bits, which is a deliberate deviation from the RC. Key distribution, forward secrecy and traffic analysis remain open. |
