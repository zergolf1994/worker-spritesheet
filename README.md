# Worker Spritesheet

Queue-based spritesheet worker สำหรับ [VdoHide](https://vdohide.xyz) — สร้าง sprite sheet (6×6, 1 เฟรม/วินาที) + `sprite.vtt` จากวิดีโอบน **local storage ของเครื่องตัวเอง** แล้วสร้าง media record `thumbnail`

> แทนที่ `server-spritesheet` เดิมที่ scan หาไฟล์เอง — ตัวนี้รับงานจากคิวอย่างเดียว มี 2 โหมดตาม `STORAGE_ID`/`STORAGE_PATH`:
>
> | โหมด | เงื่อนไข | input | output |
> |---|---|---|---|
> | **co-located** (แนะนำ) | รันคู่ storage-node, ตั้ง `STORAGE_ID`+`STORAGE_PATH` | อ่านตรงจากดิสก์ | ย้ายเข้า `sprite/` ตรง + สร้าง thumbnail media เอง |
> | **remote pool** | เครื่องกลาง ไม่ตั้งทั้งคู่ | Local โหลดผ่าน storage-node; S3 โหลดผ่าน `originUrl` | S3 source อัปโหลด sprite กลับ S3 ถาวรและสร้าง media โดยตรง; Local source ใช้ Temp + transfer |

## Features

- **Co-located** — enqueuer จ่ายงานตาม storage ที่ video media อยู่ (`targetStorageId`) → worker claim เฉพาะงานของ storage ตัวเอง อ่าน/เขียนไฟล์ผ่าน path ตรง ไม่มี network I/O ของตัววิดีโอเลย
- **Remote pool** — งานของ storage ที่ไม่มี worker ติดเครื่อง enqueuer จ่ายแบบไม่ผูก `targetStorageId` → Local source ส่งผลผ่าน sprite.zip/worker-transfer ส่วน S3 source ใช้ `originUrl` เป็น input และอัปโหลด `{fileId}/sprite/*` กลับ S3 เดิมโดยตรง
- **เลือกวิดีโอเล็กสุด** — 360 → 480 → 720 → 1080 → original (เฟรม sprite เล็กมาก ไม่จำเป็นต้อง decode ไฟล์ใหญ่)
- **Auto Retry + Backoff** — fail → กลับเป็น pending ใน doc เดิม (1m, 2m) ครบ 3 ครั้ง → failed ถาวร (ไฟล์ไม่ถูกแตะ — วิดีโอยังเล่นได้ปกติ แค่ไม่มี preview thumbnail)
- **Instant Cancel** — admin เซ็ต `status: cancelled` → context ยกเลิก → เก็บกวาด temp
- **Storage gate** — storage ตัวเองถูกปิด/เต็ม/ออฟไลน์ใน DB → หยุดหยิบงาน
- **Graceful Shutdown** — SIGTERM → คืนงานเข้าคิว (Release) + mark worker offline
- **Heartbeat** — รายงานเข้า `workers` ทุก 1 นาที พร้อม `storageId` (enqueuer ใช้จับคู่ slot ↔ storage)
- **Step-only DB writes** — DB เขียนขอบ step: prepare 15 → generate 70 → install 90 → media 100
- **Log per job** — จบงาน → อัพ `logs/process/<slug>.log` ขึ้น S3 ที่ `logs/spritesheet/` แล้วลบ local
- **Clone propagation** — thumbnail media กระจายไปไฟล์ที่ `clonedFrom` อัตโนมัติ

## Requirements

- **MongoDB** (vdohide platform database)
- **vdohide-service** รันอยู่ (enqueuer `getSpritesheetPending` เติมคิว + reaper)
- **ffmpeg + ffprobe** (install.sh ติดตั้งให้)
- เครื่องเดียวกับ **storage-node** — `STORAGE_ID` ชี้ record ใน `storages`, `STORAGE_PATH` คือโฟลเดอร์ไฟล์จริง

---

## Installation (Linux Server)

### One-line install

```bash
curl -fsSL https://raw.githubusercontent.com/zergolf1994/worker-spritesheet/main/install.sh | sudo -E bash -s -- \
    --database-url "mongodb+srv://user:pass@cluster.mongodb.net/platform" \
    --storage-id "your-storage-uuid" \
    --storage-path "/home/files" \
    -n 1
```

### Options

| Option | Default | คำอธิบาย |
|---|---|---|
| `-n, -w, --count` | `1` | จำนวน worker instances |
| `--database-url` | `""` | MongoDB connection string (`DATABASE_URL`) |
| `--storage-id` | — | local storage ที่เครื่องนี้ดูแล (**ไม่ใส่ = remote pool mode**) |
| `--storage-path` | `/home/files` | Local storage path (ใช้เมื่อมี `--storage-id`) |
| `--uninstall` | — | ถอนการติดตั้ง |

### After install

```bash
# ดู logs
journalctl -u "worker-spritesheet@*" -f

# Restart workers
for i in $(seq 1 2); do systemctl restart worker-spritesheet@$i; done

# Stop workers (SIGTERM → คืนงานเข้าคิวก่อนปิด)
for i in $(seq 1 2); do systemctl stop worker-spritesheet@$i; done
```

---

## Configuration (.env)

```env
# Required
DATABASE_URL=mongodb+srv://user:pass@cluster.mongodb.net/platform
STORAGE_ID=your-storage-uuid
STORAGE_PATH=/home/files

# Optional — Worker ID (default: spritesheet_hostname@1)
WORKER_ID=spritesheet_myhost@1

# Optional — log file (default: logs/worker-spritesheet.log)
LOG_PATH=logs/worker-spritesheet.log
```

> `STORAGE_ID` + `STORAGE_PATH` ต้องตั้ง**คู่กัน** (co-located) หรือเว้น**ทั้งคู่** (remote pool)
> — ตั้งมาตัวเดียว binary จะ exit ทันที

**Job Lifecycle เพิ่มเติมของ remote:** ถ้าไฟล์มี sprite.zip ingest ค้างรอ worker-transfer อยู่ → worker จะ complete งานเฉยๆ ไม่ทำซ้ำ (enqueuer ก็กรองไฟล์กลุ่มนี้ออกแล้วเช่นกัน)

---

## Development

```bash
git clone https://github.com/zergolf1994/worker-spritesheet.git
cd worker-spritesheet

# สร้าง .env แล้วใส่ DATABASE_URL + STORAGE_ID + STORAGE_PATH (ต้องมี ffmpeg ใน PATH)

# Run
go run ./cmd

# Build (Windows exe + copy .env → .build/)
build.bat
```

## Release

```bash
git tag v1.0.0
git push origin v1.0.0
```

GitHub Actions build + release อัตโนมัติ: `linux` (amd64), `linux-arm64`

---

## Architecture

```
vdohide-service (Node)                     worker-spritesheet (Go, ตัวนี้)
├── enqueuer:spritesheet                   ├── heartbeat (ทุก 1m → workers + storageId)
│   ไฟล์ ready ที่มี video media           ├── job loop
│   แต่ยังไม่มี thumbnail                  │   gate: storage ตัวเองพร้อมไหม
│   → targetStorageId = storage ของ media  │   Claim (targetStorageId = ของเรา)
└── reaper                                 │   → prepare (path ตรงจากดิสก์)
    processing ค้าง (heartbeat ขาด)         │   → ffmpeg generate → install → media
    → คืน pending                           │   → Complete
                                           └── cancel watcher (ทุก 5s ระหว่างมีงาน)
```

## Job Lifecycle

```
pending ──claim──▶ processing ──สำเร็จ──▶ completed
   ▲                   │
   │◀── retry (backoff 1m/2m, ≤3) ── fail
   │◀── Release (shutdown / storage ไม่พร้อม / media ย้าย storage / reaper)
   │
   └── admin เซ็ต cancelled ──▶ หยุดใน ≤5s + cleanup
       fail ครั้งที่ 3 ──▶ failed ถาวร (admin สั่ง retry เอง)
```

## Spritesheet Flow (1 job = 1 file)

1. **prepare (15%)** — หา video media เล็กสุดของไฟล์ → ตรวจว่าอยู่ storage ตัวเอง → input = `{STORAGE_PATH}/{fileId}/{fileName}` (ไม่เจอไฟล์ = fail)
2. **generate (70%)** — ffmpeg: fps 1/1s, สูง 168px, tile 6×6 ขนาดคงที่ (แผ่นสุดท้ายคง padding ไว้) → `sprite-1.jpg`, … + เขียน `sprite.vtt`
3. **install (90%)** — ย้ายเข้า `{STORAGE_PATH}/{fileId}/sprite/`
4. **media (100%)** — สร้าง media `thumbnail` (fileName `sprite.vtt`) + clone propagation

> ถ้าไฟล์มี thumbnail media อยู่แล้ว (เช่น transfer เพิ่งติดตั้ง sprite.zip จากระบบเก่า) → complete เฉยๆ ไม่ทำซ้ำ

## Collections Used

| Collection | การใช้งาน |
|---|---|
| `video_process` | คิวงาน — claim (กรอง `targetStorageId`)/settle/timeline |
| `workers` | heartbeat + `storageId`, สถานะ, system info |
| `files` | อ่าน metadata (duration) |
| `storages` | local storage ของตัวเอง (gate), S3 temp (อัพ log) |
| `medias` | หา video media เล็กสุด, สร้าง thumbnail + clone |
| `settings` | `spritesheet_config.enabled` (kill switch) |

> ⚠ **Index ทั้งหมดเป็นของฝั่ง vdohide-service (mongoose)** — repo นี้ไม่สร้าง index เอง
