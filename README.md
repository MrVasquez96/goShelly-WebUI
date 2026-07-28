# goShelly-webUI

Control Shelly Pro relays from a Raspberry Pi — over the web, or with physical buttons wired to the GPIO header. 

---

## What it does

**Relay control** — a dashboard at `/` showing live status, power and telemetry for each configured Shelly, with on/off/toggle.

**Physical buttons** — map any GPIO pin to any relay and choose how the button behaves. Debounced in the kernel, so no bounce filtering in userspace.

**GPIO visualizer** — a live 40-pin header map at `/gpio`. Press a button and it tells you which pin it's on. Built for exactly the case where you have an old board and no idea what's wired where.

---

## Requirements

- Raspberry Pi with a 40-pin header
- Go 1.24+ to build
- One or more Shelly Gen2 devices (developed against Shelly Pro 1PM, firmware 2.0.0)
- The user running the service must be in the `gpio` group

---

## How the network is wired

Shelly devices can run their own access point *and* join another AP at the same time. If each device joins the other's AP with a static address, then joining **either** AP reaches **both** devices — no router, no infrastructure, nothing else to fail.


Both relays are reachable on `192.168.34.x` while the Pi keeps its normal LAN connection on `eth0`.

The wlan connection **must not** take the default route, or you lose your LAN path to the Pi:

```bash
sudo nmcli con add type wifi con-name shelly-net ifname wlan0 \
  ssid "ShellyPro1PM-XXXXXXXXXXXX" \
  wifi-sec.key-mgmt wpa-psk wifi-sec.psk "your-ap-password" \
  ipv4.method auto ipv4.never-default yes ipv4.route-metric 700 \
  ipv6.method disabled connection.autoconnect yes

sudo nmcli con up shelly-net
```

Verify `ip route` still shows your `eth0` gateway as `default`.

> **Set the WiFi country**, or the radio comes back rfkill-soft-blocked after every reboot and the relays go unreachable:
> ```bash
> sudo raspi-config nonint do_wifi_country NO   # your ISO country code
> ```

---

## Quick start

```bash
git clone https://github.com/MrVasquez96/goShelly-webUI
cd goShelly-webUI

# 1. point config.pi.json at your relays
nano config.json

# 2. build, upload and install as a systemd service
SHELLY_UI_PASSWORD='pick-a-password' PI_PASS='your-pi-password' ./deploy.sh <RPI-IP>
```

Then open `http://<pi>:8080/` and log in as `shelly` with the password you chose.

`deploy.sh` cross-compiles for `linux/arm64`, uploads to `/home/pi/goshelly`, and installs a hardened systemd unit. It never overwrites `bindings.json`, so your button setup survives redeploys.

To run locally against a Shelly on LAN instead:

```bash
go build -o goshelly .
SHELLY_UI_PASSWORD=secret ./goshelly -config config.json
```

---

## Configuration

`config.json` / `config.pi.json`:

```json
{
  "listen": "0.0.0.0:8080",
  "refresh_seconds": 5,
  "request_timeout_seconds": 5,
  "ui_username": "shelly",
  "ui_password": "${SHELLY_UI_PASSWORD}",
  "devices": [
    { "id": "main",      "name": "Main relay",      "url": "http://192.168.34.2" },
    { "id": "secondary", "name": "Secondary relay", "url": "http://192.168.34.1" }
  ],
  "gpio": {
    "enabled": true,
    "chip": "gpiochip0",
    "default_bias": "pull-up",
    "debounce_ms": 20,
    "bindings_path": "/home/pi/goshelly/bindings.json"
  }
}
```

Any value written as `${NAME}` is read from that environment variable — keep passwords out of the file.

| Key | Default | Notes |
| --- | --- | --- |
| `listen` | `127.0.0.1:8080` | Use `0.0.0.0:8080` to reach it from your LAN. Set a password if you do. |
| `ui_password` | *(none)* | Basic auth. Without it the UI is open to anyone who can reach the port. |
| `devices[].url` | — | Device address. `username`/`password` only if the Shelly has auth enabled. |
| `gpio.enabled` | `true` | Set `false` to run relay-only, e.g. on a non-Pi host. |
| `gpio.default_bias` | `pull-up` | `pull-up`, `pull-down` or `disabled`. |
| `gpio.debounce_ms` | `20` | Kernel-level debounce. |
| `gpio.pins` | all 28 header GPIOs | Restrict if something else on the board needs a line. |

A host with no GPIO chip is not an error — the relay UI still works and `/gpio` explains why it's empty.

---

## Identifying unknown buttons

This is the workflow the visualizer exists for.

1. Open `/gpio`.
2. Look at the header map. 
3. Press a button. The **Identify** panel names the pin immediately, and the pin flashes on the header map.
4. Click **Bind GPIO n to a relay**, pick the relay and behaviour, then **Save bindings**.

### Behaviours

| Mode | On press | On release |
| --- | --- | --- |
| `toggle` | flip the relay | — |
| `momentary` | relay **on** | relay **off** |
| `momentary_inverse` | relay **off** | relay **on** |
| `on` | relay **on** | — |
| `off` | relay **off** | — |

Several bindings may share one pin, so a single button can drive both relays at once.

`bindings.json`:

```json
{
  "bindings": [
    {
      "id": "btn1",
      "name": "Kitchen",
      "gpio": 20,
      "device_id": "main",
      "switch_id": 0,
      "mode": "toggle",
      "active_low": false,
      "enabled": true,
      "bias": "pull-up",
      "debounce_ms": 25
    }
  ]
}
```

Edit it through the UI — the file is rewritten atomically on save.

---

## HTTP API

Auth is HTTP Basic. Every state-changing request needs `X-Shelly-Control: 1`, which stops a browser on another site from firing requests at it.

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/` | Relay dashboard |
| `GET` | `/gpio` | GPIO visualizer and binding editor |
| `GET` | `/healthz` | Liveness, unauthenticated |
| `GET` | `/api/devices` | Status of every device |
| `GET` | `/api/devices/{id}` | Full snapshot: config, methods, components |
| `POST` | `/api/devices/{id}/relay` | `{"on": true, "switch_id": 0, "toggle_after": 30}` |
| `POST` | `/api/devices/{id}/toggle` | `{"switch_id": 0}` |
| `GET` | `/api/gpio` | Header layout and live pin state |
| `GET` | `/api/gpio/stream` | Server-sent events, one per edge |
| `POST` | `/api/gpio/config` | `{"all": true, "bias": "pull-down", "debounce_ms": 30}` |
| `POST` | `/api/gpio/reset` | Zero the edge counters |
| `GET`/`PUT` | `/api/bindings` | Read or replace all bindings |

```bash
curl -u shelly:pass -H 'X-Shelly-Control: 1' -H 'Content-Type: application/json' \
  -X POST -d '{"on":true}' http://<RPI-IP>:8080/api/devices/main/relay
```

Relay writes wait for the device to confirm the new state before responding, so the returned `switch_status` reflects reality rather than the value from before the command.

---

## Operating it

```bash
sudo systemctl status goshelly
sudo journalctl -u goshelly -f
sudo systemctl restart goshelly
```

Successful button presses are silent; failures are logged with the binding id and pin.

---

## Design notes

- **One line request per pin.** A line held by another driver costs you that pin only, not the whole header. It also lets bias and debounce change per pin at runtime.
- **One worker goroutine per binding.** A slow or failing relay call can never reorder a press/release pair, and never blocks the GPIO reader.
- **Kernel debounce** via the gpiod v2 uAPI, with automatic fallback if the kernel is too old.
- **Edges stream over SSE**, so the visualizer reacts on press rather than on a poll interval.

Built on [go-gpiocdev](https://github.com/warthog618/go-gpiocdev) — pure Go, character-device GPIO, no `/sys/class/gpio`.

---

## Troubleshooting

**Pins all read HIGH and nothing happens on press.** Nothing is attached, or the button is wired to 3V3 while the pin is pulled up. Switch bias to pull-down in the header panel and press again.

**A binding shows `pin not claimed`.** Another driver holds that line — check `gpioinfo`. I²C, SPI and UART pins are the usual culprits.

**Relays unreachable after a reboot.** Check `nmcli dev status` for `wlan0`. If the radio is blocked, the WiFi country is probably unset — see above.

**Nothing on `/gpio`.** Confirm the service user is in the `gpio` group (`id pi`) and that `/dev/gpiochip0` exists.

**Buttons fire on release instead of press.** The `active_low` setting is inverted — see the polarity table.

---

## Security

The UI controls mains relays. Treat it accordingly:

- Always set `ui_password` when listening on anything other than loopback. The app warns at startup if you don't.
- Keep secrets in the environment via `${VAR}`, not in committed files. `deploy.sh` refuses to run without `SHELLY_UI_PASSWORD`.
- `bindings.json` and the deployed `env` file are written `0600`.
- There is no TLS. Run it on a trusted network, or put it behind a reverse proxy that terminates HTTPS.
