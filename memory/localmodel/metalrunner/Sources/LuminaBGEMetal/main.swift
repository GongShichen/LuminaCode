import Foundation
import MLX
import MLXEmbedders
import MLXLMCommon

private let embeddingDimensions = 1024
private let padTokenID = 1

private struct Request: Decodable {
    let id: Int
    let inputIDs: [[Int32]]
    let attentionMask: [[Int32]]
    let specialMask: [[Int32]]
    let includeMulti: Bool

    enum CodingKeys: String, CodingKey {
        case id
        case inputIDs = "input_ids"
        case attentionMask = "attention_mask"
        case specialMask = "special_mask"
        case includeMulti = "include_multi"
    }
}

private struct SparseToken: Encodable {
    let tokenID: Int32
    let position: Int
    let weight: Float

    enum CodingKeys: String, CodingKey {
        case tokenID = "token_id"
        case position
        case weight
    }
}

private struct TokenVector: Encodable {
    let tokenID: Int32
    let position: Int
    let weight: Float
    let values: [Float]

    enum CodingKeys: String, CodingKey {
        case tokenID = "token_id"
        case position
        case weight
        case values
    }
}

private struct Embedding: Encodable {
    let dense: [Float]
    let sparse: [SparseToken]
    let multi: [TokenVector]?
}

private struct Response: Encodable {
    let id: Int
    let embeddings: [Embedding]?
    let error: String?
    let elapsedNanos: UInt64

    enum CodingKeys: String, CodingKey {
        case id
        case embeddings
        case error
        case elapsedNanos = "elapsed_nanos"
    }
}

private final class BGEModel {
    private let model: BertModel
    private let sparseWeight: MLXArray
    private let sparseBias: MLXArray
    private let colbertWeight: MLXArray
    private let colbertBias: MLXArray

    init(modelDirectory: URL) throws {
        let backbone = modelDirectory.appendingPathComponent("metal")
        let configURL = backbone.appendingPathComponent("config.json")
        let configData = try Data(contentsOf: configURL)
        let decoder = JSONDecoder()
        let modelConfiguration = try decoder.decode(BertConfiguration.self, from: configData)
        let baseConfiguration = try decoder.decode(BaseConfiguration.self, from: configData)
        model = BertModel(modelConfiguration)
        try loadWeights(
            modelDirectory: backbone,
            model: model,
            perLayerQuantization: baseConfiguration.perLayerQuantization
        )

        let heads = modelDirectory.appendingPathComponent("heads")
        sparseWeight = try Self.loadFP16(
            heads.appendingPathComponent("sparse.weight.fp16"),
            shape: [1, embeddingDimensions]
        )
        sparseBias = try Self.loadFP16(
            heads.appendingPathComponent("sparse.bias.fp16"),
            shape: [1]
        )
        colbertWeight = try Self.loadFP16(
            heads.appendingPathComponent("colbert.weight.fp16"),
            shape: [embeddingDimensions, embeddingDimensions]
        )
        colbertBias = try Self.loadFP16(
            heads.appendingPathComponent("colbert.bias.fp16"),
            shape: [embeddingDimensions]
        )
        eval(sparseWeight, sparseBias, colbertWeight, colbertBias)
    }

    func encode(_ request: Request) throws -> [Embedding] {
        guard !request.inputIDs.isEmpty else { return [] }
        guard request.inputIDs.count == request.attentionMask.count,
            request.inputIDs.count == request.specialMask.count
        else {
            throw RuntimeError.invalidRequest("batch arrays have different lengths")
        }
        let sequenceLength = request.inputIDs[0].count
        guard sequenceLength > 0 else {
            throw RuntimeError.invalidRequest("empty token sequence")
        }
        for index in request.inputIDs.indices {
            guard request.inputIDs[index].count == sequenceLength,
                request.attentionMask[index].count == sequenceLength,
                request.specialMask[index].count == sequenceLength
            else {
                throw RuntimeError.invalidRequest("batch is not padded to a common length")
            }
        }

        let flatIDs = request.inputIDs.flatMap { $0 }
        let flatMask = request.attentionMask.flatMap { $0 }
        let positionIDs = request.attentionMask.flatMap(Self.positionIDs)
        let shape = [request.inputIDs.count, sequenceLength]
        let ids = MLXArray(flatIDs, shape)
        let mask = MLXArray(flatMask, shape)
        let positions = MLXArray(positionIDs, shape)
        let tokenTypes = MLXArray.zeros(shape, type: Int32.self)
        guard let hidden = model(
            ids,
            positionIds: positions,
            tokenTypeIds: tokenTypes,
            attentionMask: mask
        ).hiddenStates else {
            throw RuntimeError.inference("BGE-M3 did not return hidden states")
        }

        let hidden32 = hidden.asType(.float32)
        let dense = l2Normalize(hidden32[0..., 0, 0...])
        let sparse = maximum(
            matmul(hidden32, sparseWeight.asType(.float32).transposed())
                + sparseBias.asType(.float32),
            MLXArray(Float(0))
        )
        var projected: MLXArray?
        if request.includeMulti {
            projected = l2Normalize(
                matmul(hidden32, colbertWeight.asType(.float32).transposed())
                    + colbertBias.asType(.float32)
            )
        }
        if let projected {
            eval(dense, sparse, projected)
        } else {
            eval(dense, sparse)
        }

        let denseValues = dense.asArray(Float.self)
        let sparseValues = sparse.asArray(Float.self)
        let multiValues = projected?.asArray(Float.self)
        var result = [Embedding]()
        result.reserveCapacity(request.inputIDs.count)
        for batch in request.inputIDs.indices {
            let denseStart = batch * embeddingDimensions
            let sequenceStart = batch * sequenceLength
            let denseVector = Array(denseValues[denseStart ..< denseStart + embeddingDimensions])
            var sparseTokens = [SparseToken]()
            var multiTokens = [TokenVector]()
            for position in 0 ..< sequenceLength {
                let offset = sequenceStart + position
                if request.attentionMask[batch][position] == 0
                    || request.specialMask[batch][position] != 0
                {
                    continue
                }
                let weight = sparseValues[offset]
                if weight > 0 {
                    sparseTokens.append(SparseToken(
                        tokenID: request.inputIDs[batch][position],
                        position: position,
                        weight: weight
                    ))
                }
                if let multiValues {
                    let vectorStart = offset * embeddingDimensions
                    multiTokens.append(TokenVector(
                        tokenID: request.inputIDs[batch][position],
                        position: position,
                        weight: weight,
                        values: Array(
                            multiValues[vectorStart ..< vectorStart + embeddingDimensions]
                        )
                    ))
                }
            }
            result.append(Embedding(
                dense: denseVector,
                sparse: sparseTokens,
                multi: request.includeMulti ? multiTokens : nil
            ))
        }
        return result
    }

    private static func positionIDs(_ attention: [Int32]) -> [Int32] {
        var current = Int32(padTokenID)
        return attention.map { active in
            if active == 0 { return Int32(padTokenID) }
            current += 1
            return current
        }
    }

    private static func loadFP16(_ url: URL, shape: [Int]) throws -> MLXArray {
        let data = try Data(contentsOf: url)
        let count = shape.reduce(1, *)
        guard data.count == count * MemoryLayout<UInt16>.size else {
            throw RuntimeError.invalidAsset(
                "\(url.lastPathComponent) has \(data.count) bytes; expected \(count * 2)"
            )
        }
        var values = [Float]()
        values.reserveCapacity(count)
        data.withUnsafeBytes { raw in
            for index in 0 ..< count {
                let bits = raw.loadUnaligned(
                    fromByteOffset: index * MemoryLayout<UInt16>.size,
                    as: UInt16.self
                )
                values.append(Float(Float16(bitPattern: UInt16(littleEndian: bits))))
            }
        }
        return MLXArray(values, shape).asType(.float16)
    }
}

private func l2Normalize(_ value: MLXArray) -> MLXArray {
    let norm = MLXLinalg.norm(value, ord: 2, axis: -1, keepDims: true)
    return value / MLX.maximum(norm, MLXArray(Float(1e-12)))
}

private enum RuntimeError: Error, CustomStringConvertible {
    case invalidRequest(String)
    case invalidAsset(String)
    case inference(String)

    var description: String {
        switch self {
        case .invalidRequest(let value): "invalid request: \(value)"
        case .invalidAsset(let value): "invalid model asset: \(value)"
        case .inference(let value): "inference failed: \(value)"
        }
    }
}

private func writeResponse(_ response: Response) throws {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.withoutEscapingSlashes]
    let content = try encoder.encode(response)
    FileHandle.standardOutput.write(content)
    FileHandle.standardOutput.write(Data([0x0A]))
}

private func serve(modelDirectory: URL) throws {
    let model = try BGEModel(modelDirectory: modelDirectory)
    let decoder = JSONDecoder()
    while let line = readLine(strippingNewline: true) {
        let started = DispatchTime.now().uptimeNanoseconds
        var responseID = 0
        do {
            let request = try decoder.decode(Request.self, from: Data(line.utf8))
            responseID = request.id
            let embeddings = try model.encode(request)
            try writeResponse(Response(
                id: request.id,
                embeddings: embeddings,
                error: nil,
                elapsedNanos: DispatchTime.now().uptimeNanoseconds - started
            ))
        } catch {
            try writeResponse(Response(
                id: responseID,
                embeddings: nil,
                error: String(describing: error),
                elapsedNanos: DispatchTime.now().uptimeNanoseconds - started
            ))
        }
    }
}

private func main() throws {
    let arguments = CommandLine.arguments
    guard arguments.count == 3, arguments[1] == "serve" else {
        FileHandle.standardError.write(
            Data("usage: lumina-bge-metal serve <model-dir>\n".utf8)
        )
        exit(2)
    }
    try serve(modelDirectory: URL(filePath: arguments[2], directoryHint: .isDirectory))
}

do {
    try main()
} catch {
    FileHandle.standardError.write(Data("lumina-bge-metal: \(error)\n".utf8))
    exit(1)
}
