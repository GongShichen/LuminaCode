// swift-tools-version: 6.1

import PackageDescription

let package = Package(
    name: "LuminaBGEMetal",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "lumina-bge-metal", targets: ["LuminaBGEMetal"])
    ],
    dependencies: [
        .package(
            url: "https://github.com/ml-explore/mlx-swift.git",
            exact: "0.31.6"
        ),
        .package(
            url: "https://github.com/ml-explore/mlx-swift-lm.git",
            revision: "12d2da081e27785bf899c378b902bcf51abbfd96"
        ),
    ],
    targets: [
        .executableTarget(
            name: "LuminaBGEMetal",
            dependencies: [
                .product(name: "MLX", package: "mlx-swift"),
                .product(name: "MLXNN", package: "mlx-swift"),
                .product(name: "MLXEmbedders", package: "mlx-swift-lm"),
                .product(name: "MLXLMCommon", package: "mlx-swift-lm"),
            ]
        )
    ]
)
