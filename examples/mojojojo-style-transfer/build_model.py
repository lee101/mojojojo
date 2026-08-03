"""Generate a tiny, deterministic ONNX color-style transform."""
from pathlib import Path

import numpy as np
import onnx
from onnx import TensorProto, helper, numpy_helper

root = Path(__file__).parent
(root / "models").mkdir(exist_ok=True)

image = helper.make_tensor_value_info("image", TensorProto.FLOAT, [1, 3, "height", "width"])
output = helper.make_tensor_value_info("styled", TensorProto.FLOAT, [1, 3, "height", "width"])
scale = numpy_helper.from_array(np.array([0.82, 1.08, 1.18], dtype=np.float32).reshape(1, 3, 1, 1), "scale")
bias = numpy_helper.from_array(np.array([0.10, -0.01, -0.07], dtype=np.float32).reshape(1, 3, 1, 1), "bias")
low = numpy_helper.from_array(np.array(0, dtype=np.float32), "low")
high = numpy_helper.from_array(np.array(1, dtype=np.float32), "high")
nodes = [
    helper.make_node("Mul", ["image", "scale"], ["scaled"]),
    helper.make_node("Add", ["scaled", "bias"], ["shifted"]),
    helper.make_node("Clip", ["shifted", "low", "high"], ["styled"]),
]
graph = helper.make_graph(nodes, "mojojojo-tiny-style", [image], [output], [scale, bias, low, high])
model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 13)], producer_name="mojojojo-example")
model.ir_version = 8
onnx.checker.check_model(model)
onnx.save(model, root / "models" / "tiny-style.onnx")
print(root / "models" / "tiny-style.onnx")
