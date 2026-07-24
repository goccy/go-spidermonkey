export default function handler(req, res) {
  res.status(200).json({ hello: "from api", method: req.method });
}
