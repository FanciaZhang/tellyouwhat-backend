#!/usr/bin/env python3
"""Generate synthetic Mandarin ASR fixtures on macOS. No network or credentials.

Pass a disposable output directory explicitly. Requires system say and ffmpeg.
The six voices may share a synthesis base; they are a stress fixture, not a
substitute for a real six-person acceptance recording.
"""
import argparse
from pathlib import Path
import subprocess
import wave

TWO = [
    ("Tingting", "上周去爬山的时候，我走在最前面。我觉得那段山路很漂亮。"),
    ("Meijia", "我走到吊桥的时候，其实非常害怕，腿都有一点发抖。"),
    ("Tingting", "后来我在桥的另一头等你，我们一起看了日落。"),
    ("Meijia", "是的，现在回忆起来我很开心，但是当时真的很害怕。"),
]
VOICES = ["Tingting", "Meijia", "Eddy (中文（中国大陆）)", "Flo (中文（中国大陆）)",
          "Grandpa (中文（中国大陆）)", "Shelley (中文（中国大陆）)"]
SIX_TEXT = [
    "我觉得我们可以早上出发，这样路上不会太堵。",
    "我比较想去湖边散步，那里的风景很漂亮。",
    "我来负责带午饭，大家有什么不能吃的吗？",
    "我觉得坐火车更轻松，开车可能会很累。",
    "我想带上相机，给大家拍一张合影。",
    "我记得湖边有一家咖啡馆，我们可以在那里休息。",
    "那我们就先讨论一下交通方式，晚上再确定。",
    "我同意坐火车，到站以后还可以一起走走。",
    "午饭我可以做三明治，再带一些水果。",
    "我查过时刻表，九点出发的那班很合适。",
    "拍完合影以后，我还想去看一下旁边的老街。",
    "好的，那我去问一下咖啡馆能不能订位。",
]


def generate(root, name, turns, rate, gap_bytes):
    frames = []
    for index, (voice, text) in enumerate(turns):
        stem = root / f"{name}-{index}"
        aiff, pcm = stem.with_suffix(".aiff"), stem.with_suffix(".pcm")
        # Never overwrite an existing file, including a user recording.
        if aiff.exists() or pcm.exists():
            raise SystemExit(f"Output exists: {stem}")
        subprocess.run(["say", "-v", voice, "-r", str(rate), "-o", aiff, text], check=True)
        subprocess.run(["ffmpeg", "-n", "-loglevel", "error", "-i", aiff,
                        "-ar", "16000", "-ac", "1", "-f", "s16le", pcm], check=True)
        frames.extend([pcm.read_bytes(), bytes(gap_bytes)])
    destination = root / f"{name}.wav"
    with destination.open("xb") as output, wave.open(output, "wb") as wav:
        wav.setparams((1, 2, 16000, 0, "NONE", "not compressed"))
        wav.writeframes(b"".join(frames))
    print(f"{destination}: {sum(map(len, frames)) / 32000:.3f} seconds")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    generate(args.output, "two-speakers", TWO, 170, 16000)
    generate(args.output, "six-speakers", [(VOICES[i % 6], t) for i, t in enumerate(SIX_TEXT)], 175, 24000)
