INSERT INTO image_templates (id, display_name, description, image_ref) VALUES
  ('cuda-base',
   'CUDA Base (Ubuntu 22.04 + CUDA 12.4)',
   'Minimal NVIDIA CUDA runtime on Ubuntu. Install PyTorch/TF via pip inside the container.',
   'flex/dev-cuda-base:dev')
ON CONFLICT (id) DO NOTHING;
