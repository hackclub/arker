# Archiver test fixtures

Every fixture here exists so the tests can stay offline. Provenance matters
when a fixture is the evidence for a claim about a platform, so each one says
where it came from and how far it can be trusted.

| File | Provenance |
| --- | --- |
| `ytdlp_info.json` | Trimmed yt-dlp `--write-info-json` record. |
| `gallery_dl_bluesky_video_sidecar.json` | **Live capture**, gallery-dl 1.32.9, 2026-08-12, anonymous: `https://bsky.app/profile/bsky.app/post/3mgdqhzxy7s2u` (public post by the official Bluesky account). Sanitized by dropping the author avatar URL and the account's chat/verification plumbing; every remaining field is exactly what gallery-dl wrote. The declared blob size (`embed.video.size` = 251820) matched the downloaded file byte for byte, which is the evidence that gallery-dl stores Bluesky's **source** video blob rather than a transcoded playback rendition. |
| `gallery_dl_reddit_video_sidecar.json` | **Synthesized**, not a live capture: reddit was rate-limiting this network's IP during the investigation. Field names and nesting are taken from gallery-dl 1.32.9 `extractor/reddit.py` (which merges reddit's submission record verbatim) and reddit's documented `secure_media.reddit_video` shape. Post/author identifiers are invented. |
| `gallery_dl_twitter_video_sidecar.json` | **Synthesized** from gallery-dl 1.32.9 `extractor/twitter.py:230-256`, which selects the highest-bitrate entry of `video_info.variants` and records `url`/`bitrate`/`duration` on the file. No live authenticated X requests were made. IDs and handles are invented. |
| `muxed_video_audio_sample.mp4` | Generated locally with ffmpeg: 0.5s of 64x64 h264 plus an 8kHz AAC tone. Stands in for a correctly muxed download so a test can assert that both a video and an audio track survive the archive pipeline. |
| `video_only_sample.mp4` | Same clip with the audio track omitted. It is the control: it proves the audio assertion in those tests can actually fail, which is what a bare DASH video download looks like. |
