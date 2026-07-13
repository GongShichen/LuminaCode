import importlib.util
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("deepseek_judge.py")
SPEC = importlib.util.spec_from_file_location("locomo_deepseek_judge", MODULE_PATH)
JUDGE = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(JUDGE)


class DeepSeekJudgeTest(unittest.TestCase):
    def test_parse_structured_response(self):
        correct, label, reasoning = JUDGE.parse_judge_content(
            '```json\n{"reasoning":"same date", "label":"CORRECT"}\n```'
        )
        self.assertTrue(correct)
        self.assertEqual(label, "CORRECT")
        self.assertEqual(reasoning, "same date")

    def test_category_mapping_and_accuracy(self):
        source = [
            {
                "sample_id": "conv-a",
                "question_index": 0,
                "category": 1,
                "f1": 0.5,
                "retrieval_ms": 1000,
                "answer_ms": 2000,
            },
            {
                "sample_id": "conv-a",
                "question_index": 1,
                "category": 3,
                "f1": 0.25,
                "retrieval_ms": 3000,
                "answer_ms": 4000,
            },
        ]
        judged = {}
        for index, item in enumerate(source):
            result = dict(item)
            result["judge"] = {"is_correct": index == 0}
            judged[JUDGE.record_key(item)] = result
        summary = JUDGE.aggregate_results(source, judged, "model", "https://example.test")
        self.assertEqual(summary["overall"]["accuracy"], 0.5)
        self.assertEqual(summary["overall"]["runner_mean_f1"], 0.375)
        self.assertEqual(summary["by_question_type"]["single_hop"]["category"], 1)
        self.assertEqual(summary["by_question_type"]["multi_hop"]["category"], 3)


if __name__ == "__main__":
    unittest.main()
