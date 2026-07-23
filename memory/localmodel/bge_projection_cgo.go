//go:build cgo

package localmodel

/*
#cgo CFLAGS: -O3
#cgo darwin CFLAGS: -DACCELERATE_NEW_LAPACK
#cgo darwin LDFLAGS: -framework Accelerate
#include <stddef.h>
#ifdef __APPLE__
#include <Accelerate/Accelerate.h>
#endif

static void lumina_bge_project(const float *input, const float *weight, const float *bias,
                               float *output, int rows, int dimensions) {
#ifdef __APPLE__
    cblas_sgemm(CblasRowMajor, CblasNoTrans, CblasTrans, rows, dimensions, dimensions,
                1.0f, input, dimensions, weight, dimensions, 0.0f, output, dimensions);
    for (int row = 0; row < rows; row++) {
        for (int column = 0; column < dimensions; column++) {
            output[row * dimensions + column] += bias[column];
        }
    }
#else
    for (int row = 0; row < rows; row++) {
        for (int output_index = 0; output_index < dimensions; output_index++) {
            float sum = bias[output_index];
            const float *weight_row = weight + output_index * dimensions;
            const float *input_row = input + row * dimensions;
            for (int input_index = 0; input_index < dimensions; input_index++) {
                sum += input_row[input_index] * weight_row[input_index];
            }
            output[row * dimensions + output_index] = sum;
        }
    }
#endif
}
*/
import "C"

import "unsafe"

func projectBGETokens(hidden [][]float32, weight, bias []float32) [][]float32 {
	if len(hidden) == 0 {
		return nil
	}
	input := make([]float32, len(hidden)*bgeDimensions)
	for index, values := range hidden {
		copy(input[index*bgeDimensions:], values)
	}
	output := make([]float32, len(input))
	C.lumina_bge_project(
		(*C.float)(unsafe.Pointer(&input[0])),
		(*C.float)(unsafe.Pointer(&weight[0])),
		(*C.float)(unsafe.Pointer(&bias[0])),
		(*C.float)(unsafe.Pointer(&output[0])),
		C.int(len(hidden)), C.int(bgeDimensions),
	)
	result := make([][]float32, len(hidden))
	for index := range result {
		result[index] = output[index*bgeDimensions : (index+1)*bgeDimensions]
		normalizeFloat32(result[index])
	}
	return result
}
