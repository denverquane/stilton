import * as THREE from 'three';
import { OrbitControls } from 'three/addons/controls/OrbitControls.js';
import { STLLoader } from 'three/addons/loaders/STLLoader.js';
import { OBJLoader } from 'three/addons/loaders/OBJLoader.js';
import { PLYLoader } from 'three/addons/loaders/PLYLoader.js';
import type { Format, Up } from './api';

/**
 * Rotations that turn a model's up axis to +Z, the print-file convention.
 * Row-major. Must match mesh.UpMatrix on the server.
 */
const UP_MATRICES: Record<Up, number[]> = {
  '': [1, 0, 0, 0, 1, 0, 0, 0, 1],
  '+z': [1, 0, 0, 0, 1, 0, 0, 0, 1],
  '-z': [1, 0, 0, 0, -1, 0, 0, 0, -1],
  '+y': [1, 0, 0, 0, 0, -1, 0, 1, 0],
  '-y': [1, 0, 0, 0, 0, 1, 0, -1, 0],
  '+x': [0, 0, -1, 0, 1, 0, 1, 0, 0],
  '-x': [0, 0, 1, 0, 1, 0, -1, 0, 0],
};

function upMatrix(up: Up) {
  const [a, b, c, d, e, f, g, h, i] = UP_MATRICES[up] ?? UP_MATRICES[''];
  return new THREE.Matrix4().set(a, b, c, 0, d, e, f, 0, g, h, i, 0, 0, 0, 0, 1);
}

const MODEL_COLOR = 0xe8a33d;
const BACKGROUND = 0x1e2127;

/**
 * Interactive 3D view of a single model. Print files are Z-up, so the camera
 * uses Z as up rather than rotating the geometry; all three loaders return
 * raw file coordinates.
 */
export class Viewer {
  private renderer: THREE.WebGLRenderer;
  private scene = new THREE.Scene();
  private camera: THREE.PerspectiveCamera;
  private controls: OrbitControls;
  private resizeObserver: ResizeObserver;
  private object?: THREE.Object3D;
  private grid?: THREE.GridHelper;

  constructor(private container: HTMLElement) {
    this.renderer = new THREE.WebGLRenderer({ antialias: true });
    this.renderer.setPixelRatio(window.devicePixelRatio);
    container.appendChild(this.renderer.domElement);

    this.scene.background = new THREE.Color(BACKGROUND);
    this.camera = new THREE.PerspectiveCamera(35, 1, 0.1, 10000);
    this.camera.up.set(0, 0, 1); // must be set before OrbitControls reads it

    this.scene.add(new THREE.HemisphereLight(0xffffff, 0x444444, 1.2));
    const key = new THREE.DirectionalLight(0xffffff, 1.6);
    key.position.set(1, -1.5, 2);
    this.camera.add(key); // light follows the camera so the visible side is always lit
    this.scene.add(this.camera);

    this.controls = new OrbitControls(this.camera, this.renderer.domElement);
    this.controls.enableDamping = true;

    this.resizeObserver = new ResizeObserver(() => this.resize());
    this.resizeObserver.observe(container);
    this.resize();
    this.renderer.setAnimationLoop(() => {
      this.controls.update();
      this.renderer.render(this.scene, this.camera);
    });
  }

  /** Loads a model from raw file bytes. Returns false if it contained no geometry. */
  /** Loads a model from file bytes, or 'ply' for the server's mesh. */
  load(data: ArrayBuffer, format: Format | 'ply', up: Up = ''): boolean {
    const object = parse(data, format);
    object.applyMatrix4(upMatrix(up));
    if (countTriangles(object) === 0) return false;
    this.setObject(object);
    return true;
  }

  dispose() {
    this.renderer.setAnimationLoop(null);
    this.resizeObserver.disconnect();
    this.controls.dispose();
    this.clear();
    this.renderer.dispose();
    this.renderer.domElement.remove();
  }

  private setObject(object: THREE.Object3D) {
    this.clear();

    // Center on X/Y and rest the model on the grid at Z=0.
    const box = new THREE.Box3().setFromObject(object);
    const center = box.getCenter(new THREE.Vector3());
    object.position.sub(new THREE.Vector3(center.x, center.y, box.min.z));
    this.object = object;
    this.scene.add(object);

    const size = box.getSize(new THREE.Vector3());
    const radius = size.length() / 2 || 1;
    const gridSize = Math.max(10, Math.ceil((Math.max(size.x, size.y) * 1.5) / 10) * 10);
    this.grid = new THREE.GridHelper(gridSize, gridSize / 10, 0x555b66, 0x333842);
    this.grid.rotation.x = Math.PI / 2; // GridHelper lies in XZ; print beds are XY
    this.scene.add(this.grid);

    const target = new THREE.Vector3(0, 0, size.z / 2);
    const distance = radius / Math.sin(THREE.MathUtils.degToRad(this.camera.fov / 2));
    const direction = new THREE.Vector3(0.6, -1, 0.7).normalize();
    this.camera.position.copy(target).addScaledVector(direction, distance);
    this.camera.near = distance / 100;
    this.camera.far = distance * 100;
    this.camera.updateProjectionMatrix();
    this.controls.target.copy(target);
    this.controls.update();
  }

  private clear() {
    for (const obj of [this.object, this.grid]) {
      if (!obj) continue;
      this.scene.remove(obj);
      obj.traverse((child) => {
        if (child instanceof THREE.Mesh || child instanceof THREE.LineSegments) {
          child.geometry.dispose();
          const materials = Array.isArray(child.material) ? child.material : [child.material];
          materials.forEach((m: THREE.Material) => m.dispose());
        }
      });
    }
    this.object = this.grid = undefined;
  }

  private resize() {
    const { clientWidth: w, clientHeight: h } = this.container;
    if (w === 0 || h === 0) return;
    this.renderer.setSize(w, h);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
  }
}

function defaultMaterial() {
  return new THREE.MeshStandardMaterial({ color: MODEL_COLOR, roughness: 0.6, metalness: 0.05 });
}

function parse(data: ArrayBuffer, format: Format | 'ply'): THREE.Object3D {
  switch (format) {
    case 'stl':
      return new THREE.Mesh(new STLLoader().parse(data), defaultMaterial());
    case 'obj': {
      const group = new OBJLoader().parse(new TextDecoder().decode(data));
      group.traverse((child) => {
        if (child instanceof THREE.Mesh) child.material = defaultMaterial();
      });
      return group;
    }
    case 'ply': {
      // The server's mesh (used for 3MFs): colored like the thumbnail when the
      // file has colors. Vertex colors multiply the material color, so that
      // must be white. Shared vertices would smooth the shading across hard
      // edges, so shade each facet flat, as STLs look.
      const geometry = new PLYLoader().parse(data);
      const material = geometry.hasAttribute('color')
        ? new THREE.MeshStandardMaterial({ color: 0xffffff, vertexColors: true, roughness: 0.6, metalness: 0.05 })
        : defaultMaterial();
      material.flatShading = true;
      return new THREE.Mesh(geometry, material);
    }
    default:
      throw new Error(`can't display .${format} files in 3D`);
  }
}

function countTriangles(object: THREE.Object3D): number {
  let n = 0;
  object.traverse((child) => {
    if (child instanceof THREE.Mesh) {
      const g = child.geometry as THREE.BufferGeometry;
      n += (g.index ? g.index.count : g.attributes.position?.count ?? 0) / 3;
    }
  });
  return n;
}
