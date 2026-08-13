#!/usr/bin/env python3
"""Write the fixtures that cannot be generated from a live extractor run.

Nine of the matrix entries are unreachable from a developer laptop:

  instagram (reel, image, carousel)  policy: Arker never makes live Instagram
                                     requests from a dev machine (account and
                                     IP throttle risk).
  x / twitter (image, video)         gallery-dl refuses without a cookie jar.
  reddit (image, gallery, video)     reddit.com blocks the .json API from
                                     datacenter and many residential IPs.
  tiktok (video, photo)              bot challenge on every anonymous request.
  facebook (video)                   login wall.
  vimeo (video)                      yt-dlp now requires credentials.
  imgur (album)                      no anonymous album listing to discover an
                                     ID from.

Each is reconstructed here from the field names Arker's own extractor-mapping
code and tests already document, so the fixture exercises the real key
resolution order rather than an idealized schema. Every value is invented; the
shape is not. See docs/testing-contract.md.

Run: scripts/build_constructed_fixtures.py
"""

import json
import os

ROOT = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "internal", "testfixtures", "testdata")

# A single provenance marker on every constructed record. Tests use it to tell
# a hand-built fixture from a sanitized live capture, and a reader grepping the
# corpus can see immediately which is which.
MARK = "constructed-fixture: shape documented in internal/archivers, values invented"


def write(path, obj):
    full = os.path.join(ROOT, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "w") as fh:
        json.dump(obj, fh, indent=2, sort_keys=True, ensure_ascii=False)
        fh.write("\n")
    print("wrote", path)


def ytdlp(**over):
    """The subset of yt-dlp's info dict that BuildYtDlpVideoArtifacts reads.

    Field list mirrors internal/archivers/video_metadata.go:111-140 plus the
    media facts it back-fills from (width/height/fps/vcodec/acodec/tbr).
    """
    base = {
        "_arker_fixture": MARK,
        "_type": "video",
        "acodec": "mp4a.40.2",
        "age_limit": 0,
        "aspect_ratio": 0.56,
        "comment_count": 118,
        "duration": 47.0,
        "ext": "mp4",
        "format": "download-1 - 1080x1920",
        "format_id": "download-1",
        "fps": 30,
        "height": 1920,
        "like_count": 9312,
        "protocol": "https",
        "resolution": "1080x1920",
        "tbr": 2412.5,
        "vcodec": "avc1.640028",
        "view_count": 402118,
        "width": 1080,
    }
    base.update(over)
    return base


# ---------------------------------------------------------------- yt-dlp ----

write("ytdlp/vimeo_video.info.json", ytdlp(
    id="76979871",
    display_id="76979871",
    title="The New Vimeo Player (You Know, For Videos)",
    description="Vimeo's player, rebuilt. This fixture stands in for a "
                "credentialed Vimeo extraction.",
    webpage_url="https://vimeo.com/76979871",
    extractor="vimeo",
    extractor_key="Vimeo",
    uploader="Vimeo Staff",
    uploader_id="staff",
    uploader_url="https://vimeo.com/staff",
    channel="Vimeo Staff",
    channel_id="staff",
    timestamp=1382140800,
    upload_date="20131019",
    duration=63.0,
    width=1920, height=1080, aspect_ratio=1.78, resolution="1920x1080",
    tags=["vimeo", "player"],
    categories=["Documentary"],
    # Vimeo serves progressive MP4 from akamaized/vimeocdn hosts.
    url="https://vod-progressive.akamaized.net/exp/video.mp4"
        "?token=SYNTHETIC-TOKEN-NOT-A-REAL-SECRET",
))

write("ytdlp/instagram_reel.info.json", ytdlp(
    id="DbktPO1Eopi",
    display_id="DbktPO1Eopi",
    title="Agent Verse on Instagram: the news community is buzzing",
    description="the news community is buzzing #coding #devops",
    webpage_url="https://www.instagram.com/reel/DbktPO1Eopi/",
    extractor="instagram",
    extractor_key="Instagram",
    uploader="agentverseinsta",
    uploader_id="58392011",
    channel="agentverseinsta",
    timestamp=1785866323,
    upload_date="20260804",
    duration=28.0,
    view_count=1204551,
    like_count=42311,
    comment_count=903,
    tags=["coding", "devops"],
    # Instagram media comes off *.cdninstagram.com, which Arker treats as a
    # signed-media host and redacts wholesale.
    url="https://scontent-iad3-1.cdninstagram.com/o1/v/t16/f1/m86/reel.mp4"
        "?efg=SYNTHETIC-EFG-NOT-A-REAL-SECRET"
        "&_nc_ht=SYNTHETIC-NC-HT-NOT-A-REAL-SECRET"
        "&oh=SYNTHETIC-OH-NOT-A-REAL-SECRET",
))

write("ytdlp/tiktok_video.info.json", ytdlp(
    id="7106594312292453675",
    display_id="7106594312292453675",
    title="the only sound that matters",
    description="the only sound that matters #fyp",
    webpage_url="https://www.tiktok.com/@arkerfixture/video/7106594312292453675",
    extractor="tiktok",
    extractor_key="TikTok",
    uploader="arkerfixture",
    uploader_id="6141970264348852229",
    creator="Arker Fixture",
    channel="arkerfixture",
    timestamp=1654646400,
    upload_date="20220608",
    duration=15.0,
    view_count=8801244,
    like_count=1204331,
    comment_count=8812,
    repost_count=44021,
    tags=["fyp"],
    track="original sound",
    artist="arkerfixture",
    url="https://v16-webapp.tiktok.com/video/tos/useast2a/video.mp4"
        "?a=1988&signature=SYNTHETIC-SIGNATURE-NOT-A-REAL-SECRET",
))

write("ytdlp/facebook_video.info.json", ytdlp(
    id="1004422781373452",
    display_id="1004422781373452",
    title="Arker Fixture Page posted a video",
    description="A public page video used as a Facebook extraction fixture.",
    webpage_url="https://www.facebook.com/watch/?v=1004422781373452",
    extractor="facebook",
    extractor_key="Facebook",
    uploader="Arker Fixture Page",
    uploader_id="100064011223344",
    timestamp=1750000000,
    upload_date="20250615",
    duration=94.0,
    width=1280, height=720, aspect_ratio=1.78, resolution="1280x720",
    view_count=55210,
    like_count=1802,
    comment_count=214,
    # fbcdn.net is the third signed-media host in video_metadata.go.
    url="https://video-iad3-1.xx.fbcdn.net/v/t42.1790-2/video.mp4"
        "?efg=SYNTHETIC-EFG-NOT-A-REAL-SECRET"
        "&oh=SYNTHETIC-OH-NOT-A-REAL-SECRET"
        "&oe=SYNTHETIC-OE-NOT-A-REAL-SECRET",
))


# ------------------------------------------------------------- gallery-dl ----
# gallery-dl merges the post-level record into *every* file's sidecar, so slide
# 1 carries the caption/author/date for the whole post (gallery_dl.go:376-381).
# "count" is the post's declared media count and "num" the 1-based slide index
# -- the pair G1 needs to tell a complete carousel from a partial one.

def ig_slide(num, count, ext="jpg", **over):
    slide = {
        "_arker_fixture": MARK,
        "category": "instagram",
        "subcategory": "post",
        "post_id": "3955281333542808561",
        "post_shortcode": "DbktPO1Eopi",
        "post_url": "https://www.instagram.com/p/DbktPO1Eopi/",
        "username": "agentverseinsta",
        "fullname": "Agent Verse",
        "description": "ten slides from the trip — swipe #coding #devops",
        "post_date": "2026-08-04 18:12:03",
        "likes": 42311,
        "tags": ["coding", "devops"],
        "sidecar_media_id": "395528133354280856%d" % num,
        "date": "2026-08-04 18:12:03",
        "extension": ext,
        "num": num,
        "count": count,
        "width": 1080,
        "height": 1350,
    }
    slide.update(over)
    return slide


# Single-image Instagram feed post: count == 1, so structurally complete.
write("gallerydl/instagram_image/001.jpg.json", ig_slide(
    1, 1,
    post_id="3955281333542808000",
    post_shortcode="DbkSINGLEim",
    post_url="https://www.instagram.com/p/DbkSINGLEim/",
    description="one photo, no carousel #coding",
    sidecar_media_id="3955281333542808000",
))

# Ten-slide carousel with a video at slide 4. This is the G1 fixture: a run
# that stops after slide 3 must never read fulfilled.
for i in range(1, 11):
    if i == 4:
        write("gallerydl/instagram_carousel/%03d.mp4.json" % i,
              ig_slide(i, 10, ext="mp4", width=720, height=1280,
                       video_url="https://scontent.cdninstagram.com/o1/v/slide4.mp4"
                                 "?oh=SYNTHETIC-OH-NOT-A-REAL-SECRET"))
    else:
        write("gallerydl/instagram_carousel/%03d.jpg.json" % i,
              ig_slide(i, 10))

# X/Twitter. gallery-dl nests the poster under user{} and author{} and counts
# approval as favorite_count.
def tweet(num, count, ext, **over):
    rec = {
        "_arker_fixture": MARK,
        "category": "twitter",
        "subcategory": "tweet",
        "tweet_id": 1929384756102938112,
        "retweet_id": 0,
        "quote_id": 0,
        "reply_id": 0,
        "conversation_id": 1929384756102938112,
        "date": "2026-05-02 15:04:11",
        "author": {
            "id": 44196397,
            "name": "arkerfixture",
            "nick": "Arker Fixture",
            "verified": False,
            "protected": False,
        },
        "user": {
            "id": 44196397,
            "name": "arkerfixture",
            "nick": "Arker Fixture",
        },
        "content": "a post with media attached",
        "count": count,
        "num": num,
        "hashtags": ["archiving"],
        "mentions": [],
        "favorite_count": 8123,
        "quote_count": 91,
        "reply_count": 204,
        "retweet_count": 1180,
        "lang": "en",
        "extension": ext,
        "filename": "Gm%d" % num,
    }
    rec.update(over)
    return rec


write("gallerydl/x_image/001.jpg.json",
      tweet(1, 1, "jpg", width=1600, height=900,
            content="an image post on x", type="photo"))

# X video: gallery-dl reports type "video" and a bitrate/duration pair. This is
# the G3b proof that the video *bytes* come down, not a poster frame.
write("gallerydl/x_video/001.mp4.json",
      tweet(1, 1, "mp4", width=1280, height=720,
            content="a video post on x", type="video",
            duration=34.6, bitrate=2176000,
            date_original="2026-05-02 15:04:11"))

# Reddit. Approval is "score"; gallery-dl flattens the submission record.
def reddit(num, count, ext, **over):
    rec = {
        "_arker_fixture": MARK,
        "category": "reddit",
        "subcategory": "submission",
        "id": "1abcxyz",
        "title": "A public reddit submission",
        "author": "arker_fixture",
        "author_fullname": "t2_1a2b3c",
        "subreddit": "aww",
        "permalink": "https://www.reddit.com/r/aww/comments/1abcxyz/a_public_reddit_submission/",
        "url": "https://i.redd.it/abcdef123456.jpg",
        "created_utc": 1780000000,
        "date": "2026-06-01 12:00:00",
        "score": 24518,
        "num_comments": 812,
        "over_18": False,
        "is_video": False,
        "is_gallery": False,
        "domain": "i.redd.it",
        "extension": ext,
        "num": num,
        "count": count,
        "filename": "abcdef123456",
    }
    rec.update(over)
    return rec


write("gallerydl/reddit_image/001.jpg.json", reddit(1, 1, "jpg", width=3024, height=4032))

for i in range(1, 4):
    write("gallerydl/reddit_gallery/%03d.jpg.json" % i,
          reddit(i, 3, "jpg", is_gallery=True,
                 id="1galxyz",
                 title="A three-image reddit gallery",
                 permalink="https://www.reddit.com/r/aww/comments/1galxyz/a_three_image_reddit_gallery/",
                 url="https://i.redd.it/gallery%d.jpg" % i,
                 width=2048, height=1536))

# v.redd.it: DASH with audio in a separate stream. G3c is about whether the
# stored file actually carries both; the sidecar records what reddit declared.
write("gallerydl/reddit_video/001.mp4.json",
      reddit(1, 1, "mp4", is_video=True,
             id="1vidxyz",
             title="A v.redd.it video submission",
             permalink="https://www.reddit.com/r/aww/comments/1vidxyz/a_vreddit_video_submission/",
             url="https://v.redd.it/xyz987/DASH_1080.mp4",
             domain="v.redd.it",
             width=1080, height=1920,
             duration=41,
             has_audio=True,
             dash_url="https://v.redd.it/xyz987/DASHPlaylist.mpd",
             fallback_url="https://v.redd.it/xyz987/DASH_1080.mp4"))

# Imgur: album metadata is nested; upvote_count must win over favorite_count.
for i in range(1, 4):
    write("gallerydl/imgur_album/%03d.jpg.json" % i, {
        "_arker_fixture": MARK,
        "category": "imgur",
        "subcategory": "album",
        "id": "kKu3U5%d" % i,
        "url": "https://i.imgur.com/kKu3U5%d.jpg" % i,
        "title": "",
        "description": "",
        "width": 1537,
        "height": 2048,
        "size": 481920,
        "type": "image/jpeg",
        "date": "2026-08-07 14:32:03",
        "extension": "jpg",
        "num": i,
        "count": 3,
        "album": {
            "id": "zJjxIyO",
            "title": "The Baroness",
            "description": "a three image album",
            "url": "https://imgur.com/a/zJjxIyO",
            "upvote_count": 366,
            "point_count": 342,
            "favorite_count": 0,
            "score": 0,
            "image_count": 3,
            "date": "2026-08-07 14:32:03",
            "account": {"id": 27958845, "username": "arkerfixture"},
        },
    })

# TikTok photo post (/photo/). G3a: unrouted today, so this fixture exists to
# pin the behavior once routing is added.
for i in range(1, 4):
    write("gallerydl/tiktok_photo/%03d.jpg.json" % i, {
        "_arker_fixture": MARK,
        "category": "tiktok",
        "subcategory": "post",
        "id": "7301234567890123456",
        "title": "three photo slides",
        "description": "three photo slides #photomode",
        "author": {
            "id": "6141970264348852229",
            "name": "arkerfixture",
            "nick": "Arker Fixture",
        },
        "user": "arkerfixture",
        "date": "2026-03-11 09:21:40",
        "like_count": 88120,
        "comment_count": 1204,
        "share_count": 2210,
        "view_count": 1902334,
        "tags": ["photomode"],
        "extension": "jpg",
        "num": i,
        "count": 3,
        "width": 1080,
        "height": 1440,
    })

print("\nconstructed fixtures complete")
