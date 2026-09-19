// Renders the AIU app icon: three usage bars on a graphite squircle, in the panel's
// monochrome language. No provider marks. Run: swift macos/icon/render.swift <out.png>
import AppKit
import SwiftUI

struct IconView: View {
    // Apple's macOS grid: 824pt artwork centred on a 1024pt canvas.
    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: 185, style: .continuous)
                .fill(LinearGradient(colors: [Color(white: 0.23), Color(white: 0.07)], startPoint: .top, endPoint: .bottom))
                .overlay(
                    RoundedRectangle(cornerRadius: 185, style: .continuous)
                        .fill(LinearGradient(colors: [.white.opacity(0.16), .clear], startPoint: .top, endPoint: .center))
                )
                .overlay(
                    RoundedRectangle(cornerRadius: 185, style: .continuous)
                        .strokeBorder(LinearGradient(colors: [.white.opacity(0.28), .white.opacity(0.04)], startPoint: .top, endPoint: .bottom), lineWidth: 5)
                )
                .frame(width: 824, height: 824)
                .shadow(color: .black.opacity(0.35), radius: 24, y: 14)

            VStack(alignment: .leading, spacing: 74) {
                bar(fill: 0.38, opacity: 0.55)
                bar(fill: 0.66, opacity: 0.78)
                bar(fill: 0.9, opacity: 1.0)
            }
            .frame(width: 560)
        }
        .frame(width: 1024, height: 1024)
    }

    func bar(fill: Double, opacity: Double) -> some View {
        ZStack(alignment: .leading) {
            Capsule().fill(.white.opacity(0.13))
            Capsule()
                .fill(.white.opacity(opacity))
                .frame(width: 560 * fill)
                .shadow(color: .white.opacity(0.25 * opacity), radius: 16)
        }
        .frame(height: 64)
    }
}

@MainActor func render() {
    let renderer = ImageRenderer(content: IconView())
    renderer.scale = 1
    guard let cg = renderer.cgImage else { fatalError("render failed") }
    let png = NSBitmapImageRep(cgImage: cg).representation(using: .png, properties: [:])!
    try! png.write(to: URL(fileURLWithPath: CommandLine.arguments[1]))
}
MainActor.assumeIsolated { render() }
